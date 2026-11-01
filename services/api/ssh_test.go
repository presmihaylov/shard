package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/sshd"
)

const fakeHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPpJ0Xk1M8fM6m6Hn9Fq1u3oYwqXbq8VYd3m8pD2fL9Q"

// fakeSSH answers the stream with what Explain makes of explain, then echoes it until the client closes.
type fakeSSH struct {
	explain error
	served  chan sshd.Options
}

func (f *fakeSSH) HostKey() string { return fakeHostKey }

func (f *fakeSSH) Serve(_ context.Context, conn net.Conn, opts sshd.Options) error {
	f.served <- opts
	if f.explain != nil {
		if _, err := io.WriteString(conn, opts.Explain(f.explain)+"\n"); err != nil {
			return fmt.Errorf("write the explanation: %w", err)
		}
	}
	if _, err := io.Copy(conn, conn); err != nil {
		return fmt.Errorf("echo: %w", err)
	}

	return conn.Close()
}

// seedSSH is seed with an ssh server behind the route, and the daemon log the route writes to.
func seedSSH(t *testing.T) (seeded, *fakeSSH) {
	t.Helper()

	s := seed(t)
	ssh := &fakeSSH{served: make(chan sshd.Options, 1)}
	s.log = &lockedBuffer{}
	s.server = httptest.NewServer(api.NewHandler("v-test", fakeProcess{}, s.repo, nil, s.verbs, s.stores, s.egress, ssh, nil, s.log))
	t.Cleanup(s.server.Close)

	return s, ssh
}

func TestSSHRefusesBeforeThe101(t *testing.T) {
	s, ssh := seedSSH(t)

	status, body := get(t, s.server, "/v0/sandboxes/web/ssh")
	if status != http.StatusBadRequest || !strings.Contains(errorOf(t, body).message, "shard ssh") {
		t.Errorf("a plain GET answered %d %v, want 400 that names shard ssh", status, body)
	}

	cases := map[string]struct {
		err    error
		status int
		code   string
	}{
		"a stopped sandbox": {&sandbox.StateError{Sandbox: s.stopped.ID, State: models.StateStopped, Fix: "start it again with shard start " + s.stopped.ID, Code: models.CodeSandboxNotRunning}, http.StatusConflict, "sandbox_not_running"},
		"no such sandbox":   {fmt.Errorf("sandbox nope: %w", sandboxstate.ErrNotFound), http.StatusNotFound, "not_found"},
	}
	for name, c := range cases {
		s.verbs.err = c.err

		conn, resp, err := dial(t, s, "/v0/sandboxes/nope/ssh")
		if err == nil || conn != nil {
			t.Fatalf("%s got the 101", name)
		}

		status, body := decodeRefusal(t, resp)
		if status != c.status || errorOf(t, body).code != c.code {
			t.Errorf("%s answered %d %v, want %d %s", name, status, body, c.status, c.code)
		}
		if got := resp.Header.Get(api.SSHHostKeyHeader); got != "" {
			t.Errorf("%s carried the host key %q on a refusal", name, got)
		}
	}

	select {
	case opts := <-ssh.served:
		t.Errorf("a refused route reached the ssh server, for %q", opts.Sandbox)
	default:
	}
}

// The 101 pins the host key, and the ssh server gets the bytes both ways, for the sandbox the reference resolved to.
func TestSSHUpgradesAndHandsTheStreamToTheServer(t *testing.T) {
	s, ssh := seedSSH(t)
	s.verbs.sshID = s.running.ID

	conn, resp, err := dial(t, s, "/v0/sandboxes/web/ssh")
	if err != nil {
		t.Fatalf("open the ssh route: %v (%s)", err, refusal(t, resp))
	}
	if got := resp.Header.Get(api.SSHHostKeyHeader); got != fakeHostKey {
		t.Errorf("the 101 carried host key %q, want %q", got, fakeHostKey)
	}
	if s.verbs.ref != "web" {
		t.Errorf("the route asked about %q, want the reference web", s.verbs.ref)
	}
	if opts := <-ssh.served; opts.Sandbox != s.running.ID {
		t.Errorf("the ssh server serves %q, want the id %s", opts.Sandbox, s.running.ID)
	}

	stream := websocket.NetConn(t.Context(), conn, websocket.MessageBinary)
	if _, err := io.WriteString(stream, "SSH-2.0-test\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	echoed := make([]byte, len("SSH-2.0-test\r\n"))
	if _, err := io.ReadFull(stream, echoed); err != nil {
		t.Fatalf("read the echo: %v", err)
	}
	if string(echoed) != "SSH-2.0-test\r\n" {
		t.Errorf("the server echoed %q", echoed)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// What the ssh server tells a client goes out as the public text, and the cause only to the daemon log.
func TestSSHExplainsARawErrorWithThePublicTextOnly(t *testing.T) {
	s, ssh := seedSSH(t)
	ssh.explain = errors.New("runsc exec /var/lib/shard/sandboxes/sb1: socket closed")

	conn, resp, err := dial(t, s, "/v0/sandboxes/web/ssh")
	if err != nil {
		t.Fatalf("open the ssh route: %v (%s)", err, refusal(t, resp))
	}
	<-ssh.served

	stream := websocket.NetConn(t.Context(), conn, websocket.MessageBinary)
	line := make([]byte, len(internalText)+1)
	if _, err := io.ReadFull(stream, line); err != nil {
		t.Fatalf("read the explanation: %v", err)
	}
	if string(line) != internalText+"\n" {
		t.Errorf("the client read %q, want only the generic text", line)
	}
	if logged := s.log.String(); !strings.Contains(logged, "socket closed") {
		t.Errorf("the daemon log %q, want the cause", logged)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
