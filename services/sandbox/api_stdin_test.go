package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

func TestExecClientDropReleasesAttachWithBlockedStdin(t *testing.T) {
	execClientDrop(t, true)
}

func TestExecClientDropReleasesAttachWithoutStdinData(t *testing.T) {
	execClientDrop(t, false)
}

func execClientDrop(t *testing.T, input bool) {
	t.Helper()

	svc, layers := newService(t, &recorder{}, running())
	stop := make(chan struct{})
	layers.provider.serve = func(spec models.ExecSpec) (models.ExitStatus, error) {
		spec.Report(42)
		<-stop

		return models.ExitStatus{}, nil
	}
	defer close(stop)

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	handler := api.NewHandler("test", nil, layers.repo, nil, svc, nil, nil, nil, io.Discard)
	server := httptest.NewServer(handler)
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v0/sandboxes/sandbox1/exec/" + exec.ID
	conn, response, err := websocket.Dial(t.Context(), url, nil)
	refusalStatus(t, response)
	if err != nil {
		t.Fatal(err)
	}
	if input {
		writeBlockedInput(t, conn)
	}
	if err := conn.CloseNow(); err != nil {
		t.Fatal(err)
	}
	reattachAfterDrop(t, url)
}

func writeBlockedInput(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	if err := api.Send(t.Context(), conn, api.StreamStdin, bytes.Repeat([]byte("x"), api.MaxPayload)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
}

func reattachAfterDrop(t *testing.T, url string) {
	t.Helper()
	conn := reattachSocket(t, url)
	if err := conn.CloseNow(); err != nil {
		t.Fatal(err)
	}
}

func refusalStatus(t *testing.T, response *http.Response) int {
	t.Helper()
	if response == nil {
		return 0
	}
	if response.Body != nil {
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}

	return response.StatusCode
}

func TestExecInputOverflowClosesOnlyTheAttach(t *testing.T) {
	svc, url := heldExecServer(t, false, nil)
	conn := openExecSocket(t, url)
	writeBlockedInput(t, conn)
	if err := api.Send(t.Context(), conn, api.StreamStdin, bytes.Repeat([]byte("y"), api.MaxPayload)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, _, err := api.Receive(ctx, conn)
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.StatusTryAgainLater || closed.Reason != "the exec input buffer is full" {
		t.Fatalf("the input overflow returned %v", err)
	}
	execs, err := svc.ListExecs(t.Context(), "sandbox1")
	if err != nil || len(execs) != 1 || execs[0].State != models.ExecRunning {
		t.Fatalf("the input overflow ended the command: %+v, %v", execs, err)
	}
	reattachAfterDrop(t, url)
}

func TestExecPipeAcceptsNewInputAfterAClientDrops(t *testing.T) {
	execNewInputAfterDrop(t, false)
}

func TestExecTerminalAcceptsNewInputAfterAClientDrops(t *testing.T) {
	execNewInputAfterDrop(t, true)
}

func execNewInputAfterDrop(t *testing.T, terminal bool) {
	t.Helper()
	read := make(chan struct{})
	got := make(chan []byte, 1)
	_, url := heldExecServer(t, terminal, func(spec models.ExecSpec) error {
		<-read
		data, err := readThroughMarker(spec.Stdin, []byte("new input\n"))
		got <- data
		return err
	})
	conn := openExecSocket(t, url)
	writeBlockedInput(t, conn)
	if err := conn.CloseNow(); err != nil {
		t.Fatal(err)
	}
	conn = reattachSocket(t, url)
	if err := api.Send(t.Context(), conn, api.StreamStdin, []byte("new input\n")); err != nil {
		t.Fatal(err)
	}
	if err := api.Send(t.Context(), conn, api.StreamStdinClose, nil); err != nil {
		t.Fatal(err)
	}
	close(read)
	select {
	case data := <-got:
		if !bytes.HasSuffix(data, []byte("new input\n")) {
			t.Fatalf("the later attach lost its input: %q", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the later attach could not deliver its input")
	}
}

func readThroughMarker(reader io.Reader, marker []byte) ([]byte, error) {
	var data []byte
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		data = append(data, buf[:n]...)
		if bytes.HasSuffix(data, marker) {
			return data, nil
		}
		if err != nil {
			return data, err
		}
	}
}

func heldExecServer(t *testing.T, terminal bool, input func(models.ExecSpec) error) (*sandbox.Service, string) {
	t.Helper()
	svc, layers := newService(t, &recorder{}, running())
	stop := make(chan struct{})
	layers.provider.serve = func(spec models.ExecSpec) (exit models.ExitStatus, err error) {
		restore, err := rawExecTerminal(spec)
		if err != nil {
			return models.ExitStatus{}, err
		}
		defer func() { err = errors.Join(err, restore()) }()
		spec.Report(42)
		if input != nil {
			if err := input(spec); err != nil {
				return models.ExitStatus{}, err
			}
		}
		<-stop
		return models.ExitStatus{}, nil
	}
	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"cat"}, Stdin: true, TTY: terminal})
	if err != nil {
		close(stop)
		t.Fatal(err)
	}
	server := httptest.NewServer(api.NewHandler("test", nil, layers.repo, nil, svc, nil, nil, nil, io.Discard))
	t.Cleanup(func() {
		close(stop)
		server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := svc.WaitExec(ctx, "sandbox1", exec.ID); err != nil {
			t.Error(err)
		}
	})
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v0/sandboxes/sandbox1/exec/" + exec.ID
	return svc, url
}

func openExecSocket(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, response, err := websocket.Dial(t.Context(), url, nil)
	refusalStatus(t, response)
	if err != nil {
		t.Fatal(err)
	}
	closeExecSocketOnCleanup(t, conn)
	return conn
}

func reattachSocket(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	var last int
	for ctx.Err() == nil {
		conn, response, err := websocket.Dial(ctx, url, nil)
		if err == nil {
			closeExecSocketOnCleanup(t, conn)
			return conn
		}
		last = refusalStatus(t, response)
		if last != http.StatusConflict {
			t.Fatalf("reattach failed with status %d: %v", last, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the client drop left the attach occupied: status %d", last)
	return nil
}

func rawExecTerminal(spec models.ExecSpec) (func() error, error) {
	if !spec.TTY {
		return func() error { return nil }, nil
	}
	return pty.MakeRaw(spec.Stdin)
}

func closeExecSocketOnCleanup(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	t.Cleanup(func() {
		if err := conn.CloseNow(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
}

func TestExecExplicitEOFFollowsAllEarlierInput(t *testing.T) {
	read := make(chan struct{})
	got := make(chan []byte, 1)
	_, url := heldExecServer(t, false, func(spec models.ExecSpec) error {
		<-read
		data, err := io.ReadAll(spec.Stdin)
		got <- data
		return err
	})
	conn := openExecSocket(t, url)
	first := bytes.Repeat([]byte("a"), 64<<10)
	second := bytes.Repeat([]byte("b"), 64<<10)
	for _, data := range [][]byte{first, second} {
		if err := api.Send(t.Context(), conn, api.StreamStdin, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.Send(t.Context(), conn, api.StreamStdinClose, nil); err != nil {
		t.Fatal(err)
	}
	close(read)
	select {
	case data := <-got:
		if !bytes.Equal(data, append(first, second...)) {
			t.Fatal("EOF reached the command before its earlier input")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EOF never reached the command")
	}
}

func TestExecInputAfterEOFClosesOnlyTheAttach(t *testing.T) {
	_, url := heldExecServer(t, false, nil)
	conn := openExecSocket(t, url)
	if err := api.Send(t.Context(), conn, api.StreamStdinClose, nil); err != nil {
		t.Fatal(err)
	}
	if err := api.Send(t.Context(), conn, api.StreamStdin, []byte("late")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, _, err := api.Receive(ctx, conn)
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.StatusPolicyViolation || closed.Reason != "the exec input is closed" {
		t.Fatalf("input after EOF returned %v", err)
	}
	reattachAfterDrop(t, url)
}
