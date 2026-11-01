package api

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/coder/websocket"
	"github.com/danielgtaylor/huma/v2"

	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sshd"
)

// SSHHostKeyHeader carries the daemon's ssh host key on the 101, as the authorized_keys line a client pins.
const SSHHostKeyHeader = "Shard-SSH-Host-Key"

// SSHServer speaks ssh over the stream an upgrade hands it, into the one sandbox the route named.
type SSHServer interface {
	HostKey() string
	Serve(ctx context.Context, conn net.Conn, opts sshd.Options) error
}

// describeSSH names the one answer of openSSH, the 101 whose messages carry the ssh bytes.
func describeSSH(_ huma.Registry, op *huma.Operation) {
	op.Responses["101"] = &huma.Response{
		Description: "A WebSocket whose binary messages carry one ssh connection, byte for byte. The ssh user is the Unix user commands run as; the token was the check, so ssh asks for no key or password.",
		Headers: map[string]*huma.Header{
			SSHHostKeyHeader: {Description: "The daemon's ssh host key as an authorized_keys line, the same for every sandbox, which the client pins before it speaks ssh.", Schema: text()},
		},
	}
}

// openSSH serves one ssh connection into a running sandbox over the WebSocket the client opens; every refusal comes before the 101.
func (h *Handler) openSSH(w http.ResponseWriter, r *http.Request) {
	id, err := h.lifecycle.ReadyForSSH(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	if !isHandshake(r) {
		h.writeError(w, r, &sandbox.RequestError{Err: errors.New("this route only opens a WebSocket that carries ssh: connect with shard ssh, or with ssh through shard ssh-proxy")})

		return
	}

	w.Header().Set(SSHHostKeyHeader, h.ssh.HostKey())
	conn, err := websocket.Accept(w, r, nil)
	// The library answered the handshake with its own refusal, so the daemon's log is the one place left.
	if err != nil {
		h.log.Printf("api: ssh into sandbox %s: open the WebSocket: %v", id, err)

		return
	}

	stream := websocket.NetConn(r.Context(), conn, websocket.MessageBinary)
	opts := sshd.Options{Sandbox: id, Explain: func(err error) string {
		_, code := classify(err)

		return h.message(r, code, err)
	}}
	if err := h.ssh.Serve(r.Context(), stream, opts); err != nil {
		h.log.Printf("api: ssh into sandbox %s: %v", id, err)
	}
}
