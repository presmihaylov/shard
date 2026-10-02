package supervisor_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/supervisor"
)

// A guest the cursor has not seen lands from the oldest byte it holds, a host that comes back resumes where the file ends, and a guest that holds less than the file says starts over.
func TestAFileLogResumesWhereTheFileEnds(t *testing.T) {
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, "output.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	log := &supervisor.FileLog{File: f, Cursor: filepath.Join(dir, "output.cursor")}
	// An earlier boot left its output in the file.
	if _, err := log.Write([]byte("old\n")); err != nil {
		t.Fatal(err)
	}

	resume := func(from, to, want uint64) {
		t.Helper()
		at, err := log.Resume(from, to)
		if err != nil || at != want {
			t.Fatalf("Resume(%d, %d) = %d, %v, want %d", from, to, at, err, want)
		}
	}
	resume(0, 10, 0)
	if _, err := log.Write([]byte("0123")); err != nil {
		t.Fatal(err)
	}
	resume(2, 10, 4)
	resume(0, 3, 0)
	if log.Err != nil {
		t.Fatalf("Err = %v, want none", log.Err)
	}
}

// pipeLog keeps what lands and the offsets each Resume was asked about, and signals a write.
type pipeLog struct {
	got     bytes.Buffer
	resumes [][2]uint64
	at      uint64
	wrote   chan struct{}
}

func (l *pipeLog) Write(b []byte) (int, error) {
	n, err := l.got.Write(b)
	select {
	case l.wrote <- struct{}{}:
	default:
	}

	return n, err
}

func (l *pipeLog) Resume(from, to uint64) (uint64, error) {
	l.resumes = append(l.resumes, [2]uint64{from, to})

	return l.at, nil
}

// followGuest runs guest on one end of a pipe and Logs on the other, which fails any read or write still waiting at the deadline.
func followGuest(t *testing.T, sink *pipeLog, version int, guest func(net.Conn) error) error {
	t.Helper()
	host, g := net.Pipe()
	if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		defer g.Close()
		done <- guest(g)
	}()
	err := supervisor.Logs(context.Background(), func(context.Context, uint32) (net.Conn, error) { return host, nil }, sink, version)
	if gerr := <-done; gerr != nil {
		t.Errorf("guest: %v", gerr)
	}

	return err
}

// A guest whose state names the logs version gets the host's resume byte, and an ack after each write with the output offset after it.
func TestAGuestWithTheLogsVersionResumesAndGetsAcks(t *testing.T) {
	sink := &pipeLog{at: 4}
	err := followGuest(t, sink, supervisor.LogsVersion, func(g net.Conn) error {
		if _, err := g.Write(supervisor.LogsHeader(2, 8)); err != nil {
			return err
		}
		var at uint64
		if err := binary.Read(g, binary.BigEndian, &at); err != nil {
			return err
		}
		if at != 4 {
			return errors.New("resumed at the wrong byte")
		}
		if _, err := g.Write([]byte("4567")); err != nil {
			return err
		}
		if err := binary.Read(g, binary.BigEndian, &at); err != nil {
			return err
		}
		if at != 8 {
			return errors.New("acked the wrong offset")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.got.String(); got != "4567" || len(sink.resumes) != 1 || sink.resumes[0] != [2]uint64{2, 8} {
		t.Fatalf("landed %q after resumes %v, want 4567 after [2 8]", got, sink.resumes)
	}
}

// An older guest names no logs version and sends raw output: every byte is output, even one shaped like offsets, and the host sends nothing back.
func TestAnOlderGuestsRawOutputLandsWithNoAcks(t *testing.T) {
	sink := &pipeLog{wrote: make(chan struct{}, 1)}
	raw := string(supervisor.LogsHeader(0, 6))
	err := followGuest(t, sink, 0, func(g net.Conn) error {
		if _, err := g.Write([]byte(raw)); err != nil {
			return err
		}
		select {
		case <-sink.wrote:
		case <-time.After(5 * time.Second):
			return errors.New("the host held the first write back")
		}
		// The host never acks, so a write here would block on an ack nobody reads if it did.
		_, err := g.Write([]byte("line two\n"))

		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.got.String(); got != raw+"line two\n" || len(sink.resumes) != 0 {
		t.Fatalf("landed %q after resumes %v, want every raw byte and no resume", got, sink.resumes)
	}
}

// A logs version this host does not read fails the stream with an error that names it, before the host reads or answers anything.
func TestAnUnknownLogsVersionFailsTheStreamAndNamesIt(t *testing.T) {
	sink := &pipeLog{}
	err := followGuest(t, sink, 9, func(net.Conn) error { return nil })
	if !errors.Is(err, supervisor.ErrLogsVersion) || !strings.Contains(err.Error(), "version 9") {
		t.Fatalf("Logs = %v, want the unknown version 9", err)
	}
	if sink.got.Len() != 0 || len(sink.resumes) != 0 {
		t.Fatalf("landed %q after resumes %v, want nothing", sink.got.String(), sink.resumes)
	}
}
