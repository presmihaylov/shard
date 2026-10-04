package supervisor_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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
	log := &supervisor.FileLog{File: f, Cursor: filepath.Join(dir, "output.cursor"), Max: supervisor.MaxLog}
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

// A write that would take the file past Max renames it first, and the cursor follows the output into the new file.
func TestAFileLogRotatesBeforeItPassesMax(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	log := &supervisor.FileLog{File: f, Cursor: filepath.Join(dir, "output.cursor"), Max: 8}
	defer log.Close()
	if _, err := log.Write([]byte("old\n")); err != nil {
		t.Fatal(err)
	}
	if at, err := log.Resume(0, 100); err != nil || at != 0 {
		t.Fatalf("Resume(0, 100) = %d, %v, want 0", at, err)
	}
	for _, b := range []string{"0123", "4567"} {
		if _, err := log.Write([]byte(b)); err != nil {
			t.Fatal(err)
		}
	}

	for name, want := range map[string]string{path: "4567", path + ".1": "old\n0123"} {
		got, err := os.ReadFile(name)
		if err != nil || string(got) != want {
			t.Errorf("%s holds %q, %v, want %q", filepath.Base(name), got, err, want)
		}
	}
	// The guest landed output bytes 0 to 7, so a host that comes back resumes it at 8.
	if at, err := log.Resume(2, 12); err != nil || at != 8 {
		t.Fatalf("Resume(2, 12) after the rotation = %d, %v, want 8", at, err)
	}
	if log.Err != nil {
		t.Fatalf("Err = %v, want none", log.Err)
	}
}

// A log a daemon before the bound left past Max is bounded with no write, and a FileLog over it resumes the guest where the old file ended.
func TestBoundLogBoundsALegacyLogWithNoWrite(t *testing.T) {
	dir := t.TempDir()
	path, cursor := filepath.Join(dir, "output.log"), filepath.Join(dir, "output.cursor")
	if err := os.WriteFile(path, []byte("old\n0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The file's byte 4 is the guest's output byte 0, so the file ends at output byte 10.
	if err := os.WriteFile(cursor, []byte(`{"offset":4,"output":0}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := supervisor.BoundLog(path, cursor, 8); err != nil {
		t.Fatalf("BoundLog: %v", err)
	}

	for name, want := range map[string]string{path: "", path + ".1": "23456789"} {
		got, err := os.ReadFile(name)
		if err != nil || string(got) != want {
			t.Errorf("%s holds %q, %v, want %q", filepath.Base(name), got, err, want)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	log := &supervisor.FileLog{File: f, Cursor: cursor, Max: 8}
	defer log.Close()
	if at, err := log.Resume(2, 12); err != nil || at != 10 {
		t.Fatalf("Resume(2, 12) after the bound = %d, %v, want 10", at, err)
	}
}

// A log within Max, and one that is not there, are left as they are.
func TestBoundLogLeavesALogWithinMax(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")
	if err := os.WriteFile(path, []byte("01234567"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := supervisor.BoundLog(path, filepath.Join(dir, "output.cursor"), 8); err != nil {
		t.Fatalf("BoundLog: %v", err)
	}
	if err := supervisor.BoundLog(filepath.Join(dir, "gone.log"), filepath.Join(dir, "gone.cursor"), 8); err != nil {
		t.Fatalf("BoundLog of a log that is not there: %v", err)
	}

	if got, err := os.ReadFile(path); err != nil || string(got) != "01234567" {
		t.Errorf("output.log holds %q, %v, want it whole", got, err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("output.log.1 exists: %v", err)
	}
}

// pipeLog keeps what lands and the offsets each Resume was asked about, and signals a write; refuse fails the call it names.
type pipeLog struct {
	got     bytes.Buffer
	resumes [][2]uint64
	at      uint64
	wrote   chan struct{}
	refuse  string
}

var errRefused = errors.New("the log refuses it")

func (l *pipeLog) Write(b []byte) (int, error) {
	if l.refuse == "write" {
		return 0, errRefused
	}
	n, err := l.got.Write(b)
	select {
	case l.wrote <- struct{}{}:
	default:
	}

	return n, err
}

func (l *pipeLog) Resume(from, to uint64) (uint64, error) {
	l.resumes = append(l.resumes, [2]uint64{from, to})
	if l.refuse == "resume" {
		return 0, errRefused
	}

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

// A log that refuses the resume or a write ends the stream with the stop word in place of an answer, so the guest waits for no ack of it.
func TestALogThatRefusesTheOutputTellsTheGuestItStopped(t *testing.T) {
	for _, refuse := range []string{"resume", "write"} {
		t.Run(refuse, func(t *testing.T) {
			sink := &pipeLog{refuse: refuse}
			err := followGuest(t, sink, supervisor.LogsVersion, func(g net.Conn) error {
				if _, err := g.Write(supervisor.LogsHeader(0, 4)); err != nil {
					return err
				}
				var answer uint64
				if err := binary.Read(g, binary.BigEndian, &answer); err != nil {
					return err
				}
				if refuse == "write" {
					if _, err := g.Write([]byte("0123")); err != nil {
						return err
					}
					if err := binary.Read(g, binary.BigEndian, &answer); err != nil {
						return err
					}
				}
				if answer != supervisor.LogsStopped {
					return fmt.Errorf("the host answered %d, want the stop word", answer)
				}

				return nil
			})
			if !errors.Is(err, errRefused) {
				t.Fatalf("Logs = %v, want the log's refusal", err)
			}
		})
	}
}
