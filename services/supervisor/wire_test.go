package supervisor_test

import (
	"bufio"
	"bytes"
	"context"
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

// endless is a guest that writes one line and never ends it; it gives up at 64 MiB so a reader with no bound fails the test, not the host.
type endless struct{ read int }

func (e *endless) Read(p []byte) (int, error) {
	if e.read > 64<<20 {
		return 0, errors.New("the guest gave up at 64 MiB")
	}
	for i := range p {
		p[i] = 'a'
	}
	e.read += len(p)

	return len(p), nil
}

func TestReadMessageRefusesALineThatNeverEnds(t *testing.T) {
	guest := &endless{}
	var m supervisor.Message
	if err := supervisor.ReadMessage(bufio.NewReader(guest), &m); !errors.Is(err, supervisor.ErrMessageTooLong) {
		t.Fatalf("err = %v, want ErrMessageTooLong", err)
	}
	if guest.read > 2*supervisor.MaxPayload {
		t.Errorf("the reader took %d bytes before it refused, want about %d", guest.read, supervisor.MaxPayload)
	}
}

func TestReadMessageTakesALineUpToTheBound(t *testing.T) {
	at := `"` + strings.Repeat("a", supervisor.MaxPayload-2) + `"`

	var got string
	if err := supervisor.ReadMessage(bufio.NewReader(strings.NewReader(at+"\n")), &got); err != nil {
		t.Fatalf("a line of exactly %d bytes: %v", supervisor.MaxPayload, err)
	}
	if len(got) != supervisor.MaxPayload-2 {
		t.Errorf("decoded %d bytes, want %d", len(got), supervisor.MaxPayload-2)
	}
	if err := supervisor.ReadMessage(bufio.NewReader(strings.NewReader(at+" \n")), &got); !errors.Is(err, supervisor.ErrMessageTooLong) {
		t.Fatalf("a line one byte past the bound: %v, want ErrMessageTooLong", err)
	}
}

func TestWriteMessageRefusesWhatTheReaderWould(t *testing.T) {
	var sent bytes.Buffer
	err := supervisor.WriteMessage(&sent, strings.Repeat("a", supervisor.MaxPayload))
	if !errors.Is(err, supervisor.ErrMessageTooLong) {
		t.Fatalf("err = %v, want ErrMessageTooLong", err)
	}
	if sent.Len() != 0 {
		t.Errorf("%d bytes went out before the refusal", sent.Len())
	}
}

// TestALineThatNeverEndsEndsTheControlStream is the guest of SHARD-340: the stream ends with the typed error, and a request after it is refused.
func TestALineThatNeverEndsEndsTheControlStream(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()

	go func() {
		chunk := bytes.Repeat([]byte("a"), 64<<10)
		for {
			if _, err := guest.Write(chunk); err != nil {
				return
			}
		}
	}()

	c := supervisor.ControlOver(host)
	if _, err := c.Next(); !errors.Is(err, supervisor.ErrMessageTooLong) {
		t.Fatalf("next = %v, want ErrMessageTooLong", err)
	}
	if err := c.Thaw(t.Context()); !errors.Is(err, supervisor.ErrMessageTooLong) {
		t.Fatalf("thaw after the refusal = %v, want ErrMessageTooLong", err)
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
	err := c.Run(t.Context(), supervisor.RunSpec{Argv: []string{"/missing"}})
	if !errors.Is(err, supervisor.ErrEntrypointNotStarted) || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("err = %v, want ErrEntrypointNotStarted with the guest's reason", err)
	}
}

func TestWriteExitKeepsOnlyTheLastRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.json")
	for i := range 200 {
		if err := supervisor.WriteExit(path, models.ExitStatus{Code: i % 7, Signal: 9}); err != nil {
			t.Fatal(err)
		}
	}

	got, ok, err := bundle.ReadExitStatus(path)
	if err != nil || !ok {
		t.Fatalf("read = %v %v", ok, err)
	}
	if got != (models.ExitStatus{Code: 199 % 7, Signal: 9}) {
		t.Fatalf("got %+v, want the last record", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 64 {
		t.Fatalf("the exit file is %d bytes after 200 exits, want one record", info.Size())
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

// A replay that brings the count already on disk writes nothing, so a full root fails no attach (SHARD-341).
func TestWriteRestartsLeavesTheSameCountAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "restarts.json")
	count := models.RestartCount{Count: 2, LastAt: time.Date(2026, 10, 2, 19, 0, 0, 0, time.UTC)}
	if err := supervisor.WriteRestarts(path, count); err != nil {
		t.Fatal(err)
	}

	// A directory that refuses a new file fails the write the way a full disk does.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	if err := supervisor.WriteRestarts(path, count); err != nil {
		t.Fatalf("WriteRestarts of the same count = %v, want no write at all", err)
	}
	count.Count++
	if err := supervisor.WriteRestarts(path, count); err == nil {
		t.Fatal("WriteRestarts of a new count wrote into a directory that refuses a new file")
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
	go func() { done <- c.Signal(t.Context(), 1, "KILL") }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "went away") {
			t.Fatalf("signal = %v, want the refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("signal blocked after the reader ended")
	}
}

// A guest whose vmm is stopped reads the request and never answers: the request ends at its deadline (SHARD-339).
func TestARequestTheGuestNeverAnswersEndsAtItsDeadline(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	asked := make(chan supervisor.Message, 2)
	go func() {
		r := bufio.NewReader(guest)
		for {
			var m supervisor.Message
			if err := supervisor.ReadMessage(r, &m); err != nil {
				return
			}
			asked <- m
		}
	}()

	c := supervisor.ControlOver(host)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := c.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop = %v, want the deadline", err)
	}

	stop := <-asked
	if err := supervisor.WriteMessage(guest, supervisor.Message{Kind: supervisor.KindFailure, ID: stop.ID, Error: "late"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Thaw(t.Context()) }()
	thaw := <-asked
	if err := supervisor.WriteMessage(guest, supervisor.Message{Kind: supervisor.KindDone, ID: thaw.ID}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("thaw = %v, want its own answer, not the stop's late one", err)
	}
}

func TestARequestTheGuestNeverReadsEndsAtItsDeadline(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()

	c := supervisor.ControlOver(host)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := c.Stop(ctx); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("stop = %v, want the write deadline", err)
	}
	if err := c.Thaw(t.Context()); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("thaw after a failed write = %v, want the same refusal", err)
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

// fakeExecGuest takes the header, reports a start, sends the frames given, then reports each stream the host sends until it hangs up.
func fakeExecGuest(guest net.Conn, then []byte) <-chan byte {
	got := make(chan byte, 8)
	go func() {
		defer close(got)
		var header supervisor.ExecHeader
		if err := supervisor.ReadHeader(guest, &header); err != nil {
			return
		}
		if err := supervisor.WriteJSONFrame(guest, supervisor.StreamStarted, supervisor.StartedFrame{PID: 7}); err != nil {
			return
		}
		for _, stream := range then {
			if err := supervisor.WriteFrame(guest, stream, nil); err != nil {
				return
			}
		}
		for {
			stream, _, err := supervisor.ReadFrame(guest)
			if err != nil {
				return
			}
			got <- stream
		}
	}()

	return got
}

func sawCancel(got <-chan byte) bool {
	seen := false
	for stream := range got {
		seen = seen || stream == supervisor.StreamCancel
	}

	return seen
}

// A cancel is a frame, because a connection that only drops is a daemon restart, and the guest keeps the command (SHARD-270).
func TestACancelledExecTellsTheGuest(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	got := fakeExecGuest(guest, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dial := func(context.Context, uint32) (net.Conn, error) { return host, nil }
	spec := models.ExecSpec{Report: func(int) { cancel() }}
	if _, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: []string{"sleep"}}, spec); !errors.Is(err, context.Canceled) {
		t.Fatalf("exec = %v, want context.Canceled", err)
	}
	if !sawCancel(got) {
		t.Fatal("the guest never got a cancel frame")
	}
}

func TestAnExecTheHostGivesUpOnIsCancelled(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	got := fakeExecGuest(guest, []byte{99})

	dial := func(context.Context, uint32) (net.Conn, error) { return host, nil }
	if _, err := supervisor.Exec(t.Context(), dial, "sb", supervisor.ExecHeader{Argv: []string{"sleep"}}, models.ExecSpec{}); err == nil {
		t.Fatal("exec took a frame of stream 99")
	}
	if !sawCancel(got) {
		t.Fatal("the guest never got a cancel frame")
	}
}

// FC and vz report a signalled exec by its 128+n alone, the shape runsc and runc exec give (SHARD-432).
func TestExecReportsASignalledCommandByItsCodeAlone(t *testing.T) {
	host, guest := net.Pipe()
	// Open until Exec returns: a net.Pipe fails the host's SetDeadline once either end closes, and a vsock conn does not (SHARD-471).
	defer guest.Close()
	sent := make(chan error, 1)
	go func() {
		var header supervisor.ExecHeader
		if err := supervisor.ReadHeader(guest, &header); err != nil {
			sent <- err
			return
		}
		sent <- supervisor.WriteJSONFrame(guest, supervisor.StreamExit, supervisor.ExitFrame{Code: 143, Signal: 15})
	}()

	dial := func(context.Context, uint32) (net.Conn, error) { return host, nil }
	exit, err := supervisor.Exec(t.Context(), dial, "sb", supervisor.ExecHeader{Argv: []string{"sleep", "30"}}, models.ExecSpec{})
	if gerr := <-sent; gerr != nil {
		t.Fatalf("the guest: %v", gerr)
	}
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if exit != (models.ExitStatus{Code: 143}) {
		t.Errorf("exec = %+v, want code 143 and signal 0, as runsc and runc exec report", exit)
	}
}
