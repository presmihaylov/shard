package supervisor_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

func TestFrameRoundTripSplitsAtMaxPayload(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), supervisor.MaxPayload+1)
	var buf bytes.Buffer
	if err := supervisor.WriteFrame(&buf, supervisor.StreamStdout, payload); err != nil {
		t.Fatal(err)
	}

	var got []byte
	for range 2 {
		stream, piece, err := supervisor.ReadFrame(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if stream != supervisor.StreamStdout {
			t.Fatalf("stream = %d, want stdout", stream)
		}
		got = append(got, piece...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %d bytes back, want %d", len(got), len(payload))
	}
	if _, _, err := supervisor.ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("a drained reader gave %v, want io.EOF", err)
	}
}

func TestReadFrameRefusesAnOversizeLength(t *testing.T) {
	header := []byte{supervisor.StreamStdout, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}
	_, _, err := supervisor.ReadFrame(bytes.NewReader(header))
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("err = %v, want the bound refusal", err)
	}
}

func TestReadMessageReportsAClosedPeerAsEOF(t *testing.T) {
	var m supervisor.Message
	err := supervisor.ReadMessage(bufio.NewReader(strings.NewReader("")), &m)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestReadHeaderLeavesTheFramesBehindIt(t *testing.T) {
	var buf bytes.Buffer
	if err := supervisor.WriteMessage(&buf, supervisor.ExecHeader{Argv: []string{"sh"}}); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.WriteFrame(&buf, supervisor.StreamStdin, []byte("in")); err != nil {
		t.Fatal(err)
	}

	var header supervisor.ExecHeader
	if err := supervisor.ReadHeader(&buf, &header); err != nil {
		t.Fatal(err)
	}
	if len(header.Argv) != 1 || header.Argv[0] != "sh" {
		t.Fatalf("header = %+v", header)
	}
	stream, payload, err := supervisor.ReadFrame(&buf)
	if err != nil || stream != supervisor.StreamStdin || string(payload) != "in" {
		t.Fatalf("frame = %d %q %v, want the stdin frame intact", stream, payload, err)
	}
}

func TestRunReportsAFailureAsNotStarted(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	go func() {
		defer guest.Close()
		r := bufio.NewReader(guest)
		var m supervisor.Message
		if err := supervisor.ReadMessage(r, &m); err != nil || m.Kind != supervisor.KindRun {
			return
		}
		_ = supervisor.WriteMessage(guest, supervisor.Message{Kind: supervisor.KindFailure, ID: m.ID, Error: "no such file"})
	}()

	c := supervisor.ControlOver(host)
	err := c.Run(supervisor.RunSpec{Argv: []string{"/missing"}})
	if !errors.Is(err, supervisor.ErrEntrypointNotStarted) || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("err = %v, want ErrEntrypointNotStarted with the guest's reason", err)
	}
}

func TestAppendExitReadsBackThroughBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.json")
	for _, exit := range []models.ExitStatus{{Code: 1}, {Code: 0, Signal: 9}} {
		if err := supervisor.AppendExit(path, exit); err != nil {
			t.Fatal(err)
		}
	}

	got, ok, err := bundle.ReadExitStatus(path)
	if err != nil || !ok {
		t.Fatalf("read = %v %v", ok, err)
	}
	if got.Signal != 9 {
		t.Fatalf("got %+v, want the last record", got)
	}
}

func TestWriteRestartsReadsBackThroughBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restarts.json")
	if err := supervisor.WriteRestarts(path, models.RestartCount{Count: 2, GaveUp: true}); err != nil {
		t.Fatal(err)
	}

	got, err := bundle.Bundle{RestartFile: path}.RestartCount()
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 2 || !got.GaveUp {
		t.Fatalf("got %+v", got)
	}
}

func TestRequestAfterTheReaderEndedIsRefused(t *testing.T) {
	// A unix socket half-closed by the guest still takes the host's write, so only the reader's end can refuse the request.
	dir := shortDir(t)
	l, err := net.Listen("unix", filepath.Join(dir, "c.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()
	host, err := net.Dial("unix", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	guest := (<-accepted).(*net.UnixConn)
	defer guest.Close()

	c := supervisor.ControlOver(host)
	if err := guest.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("next = %v, want io.EOF once the guest is gone", err)
	}

	done := make(chan error, 1)
	go func() { done <- c.Signal(1, "KILL") }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "went away") {
			t.Fatalf("signal = %v, want the refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("signal blocked after the reader ended")
	}
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sv") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove %s: %v", dir, err)
		}
	})

	return dir
}
