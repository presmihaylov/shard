package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// createExec validates the command and names the exec; nothing runs until a client attaches.
func (h *Handler) createExec(w http.ResponseWriter, r *http.Request) {
	var req sandbox.ExecRequest
	if err := decode(r, &req); err != nil {
		h.writeError(w, err)

		return
	}

	exec, err := h.lifecycle.CreateExec(r.Context(), r.PathValue("id"), req)
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusCreated, exec)
}

// execsResponse is a page of one sandbox's execs, oldest id first.
type execsResponse struct {
	Execs []models.Exec `json:"execs"`
	Next  *string       `json:"next"`
}

// listExecs answers a page of the sandbox's execs.
func (h *Handler) listExecs(w http.ResponseWriter, r *http.Request) {
	q, err := pageOf(r, sandbox.ValidExecID)
	if err != nil {
		h.writeError(w, err)

		return
	}

	execs, err := h.lifecycle.ListExecs(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	execs, next := page(execs, q, func(e models.Exec) string { return e.ID })

	h.writeJSON(w, http.StatusOK, execsResponse{Execs: execs, Next: next})
}

// getExec answers one exec three ways: a WebSocket upgrade attaches, ?wait=true blocks until it ends,
// and a plain GET is the record as it stands now.
func (h *Handler) getExec(w http.ResponseWriter, r *http.Request) {
	if isHandshake(r) {
		h.attachExec(w, r)

		return
	}

	wait, err := boolQuery(r, "wait")
	if err != nil {
		h.writeError(w, err)

		return
	}

	if wait {
		exec, err := h.lifecycle.WaitExec(r.Context(), r.PathValue("id"), r.PathValue("exec"))
		if err != nil {
			h.writeError(w, err)

			return
		}

		h.writeJSON(w, http.StatusOK, exec)

		return
	}

	exec, err := h.lifecycle.GetExec(r.Context(), r.PathValue("id"), r.PathValue("exec"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusOK, exec)
}

// killRequest names the signal a kill sends. An empty signal is TERM.
type killRequest struct {
	Signal string `json:"signal,omitempty"`
}

// killExec sends one signal to a running exec.
func (h *Handler) killExec(w http.ResponseWriter, r *http.Request) {
	var req killRequest
	if err := decode(r, &req); err != nil {
		h.writeError(w, err)

		return
	}

	if err := h.lifecycle.KillExec(r.Context(), r.PathValue("id"), r.PathValue("exec"), req.Signal); err != nil {
		h.writeError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// deleteExec forgets an exec that has ended and frees its buffer.
func (h *Handler) deleteExec(w http.ResponseWriter, r *http.Request) {
	if err := h.lifecycle.DeleteExec(r.Context(), r.PathValue("id"), r.PathValue("exec")); err != nil {
		h.writeError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// attachExec replays the exec's buffer to the client, then streams it live until the command ends. It is
// reached over the WebSocket the client opens, and every refusal comes before the 101.
func (h *Handler) attachExec(w http.ResponseWriter, r *http.Request) {
	// A client that hangs up drops the attach and nothing else: the command runs on to re-attach by exec id, and only stop ends a sandbox.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	session := &execSession{w: w, r: r, log: h.log, cancel: cancel}
	defer session.close()

	stdin, writer := io.Pipe()
	session.stdin = writer

	streams := sandbox.Streams{
		Stdin:   stdin,
		Stdout:  session.stream(StreamStdout),
		Stderr:  session.stream(StreamStderr),
		Started: session.start,
		Warn: func(message string) {
			h.log.Printf("api: exec %s in sandbox %s: %s", r.PathValue("exec"), r.PathValue("id"), message)
		},
	}

	exit, err := h.lifecycle.Attach(ctx, r.PathValue("id"), r.PathValue("exec"), streams)

	// Nothing was said on the wire yet, so the refusal is a status and a JSON body like every other route.
	if !session.answered {
		h.writeError(w, err)

		return
	}

	// The library answered the handshake with its own refusal, so the daemon's log is the one place left.
	if session.conn == nil {
		h.log.Printf("api: exec %s in sandbox %s: %v", r.PathValue("exec"), r.PathValue("id"), err)

		return
	}

	// The client hung up, so the command runs on and there is no live socket left to send an exit over.
	if ctx.Err() != nil {
		return
	}

	session.finish(exit, err)
}

// isHandshake reports whether the request is the WebSocket opening handshake, so a route serves the plain body without it.
func isHandshake(r *http.Request) bool {
	switch {
	case !hasToken(r.Header.Get("Connection"), "upgrade"),
		!hasToken(r.Header.Get("Upgrade"), "websocket"),
		r.Header.Get("Sec-WebSocket-Version") != "13",
		r.Header.Get("Sec-WebSocket-Key") == "":
		return false
	}

	return true
}

// hasToken says whether a comma-separated header names token, in any case.
func hasToken(header, token string) bool {
	for part := range strings.SplitSeq(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}

	return false
}

// execSession is the client side of one exec: the messages it sends in, and the ones the guest sends back.
type execSession struct {
	w      http.ResponseWriter
	r      *http.Request
	log    *log.Logger
	cancel context.CancelFunc

	// answered says the handshake was answered, in the affirmative or not, so no JSON body follows it.
	answered bool
	conn     *websocket.Conn
	stdin    *io.PipeWriter
}

// start answers the 101 and takes the connection, so everything after this is messages and never HTTP.
func (e *execSession) start(execID string) error {
	e.answered = true

	conn, err := websocket.Accept(e.w, e.r, nil)
	if err != nil {
		return fmt.Errorf("open the WebSocket of exec %s: %w", execID, err)
	}
	conn.SetReadLimit(MaxPayload + 1)
	e.conn = conn

	go e.read()

	return nil
}

// read moves the client's messages into the guest's stdin until the client says it is done, or goes away.
func (e *execSession) read() {
	for {
		stream, payload, err := Receive(context.Background(), e.conn)
		if err != nil {
			// The client is gone, so this attach ends; the command runs on and a later attach replays it.
			e.closeStdin(err)
			e.cancel()

			return
		}

		switch stream {
		case StreamStdin:
			if _, err := e.stdin.Write(payload); err != nil {
				e.log.Printf("api: exec: the command stopped reading its input: %v", err)

				return
			}
		case StreamStdinClose:
			e.closeStdin(io.EOF)
		default:
			e.log.Printf("api: exec: the client sent a message of stream %d, which no client sends", stream)
			e.closeStdin(fmt.Errorf("the client sent a message of stream %d", stream))
			e.cancel()

			return
		}
	}
}

// closeStdin hands the guest process the end of its input; CloseWithError reports nothing on a second call.
func (e *execSession) closeStdin(err error) {
	if closeErr := e.stdin.CloseWithError(err); closeErr != nil {
		e.log.Printf("api: exec: close the input of the command: %v", closeErr)
	}
}

// finish says how the command ended. A command that never ran exits with the code a shell answers for it.
func (e *execSession) finish(exit models.ExitStatus, err error) {
	var notStarted *models.CommandNotStartedError
	if errors.As(err, &notStarted) {
		e.send(StreamExit, ExitMessage{Code: notStarted.Code, Error: notStarted.Reason})

		return
	}

	if err != nil {
		e.send(StreamFailure, failureOf(err))

		return
	}

	e.send(StreamExit, ExitMessage{Code: exit.Code, Signal: exit.Signal})
}

func (e *execSession) send(stream byte, payload any) {
	if err := sendJSON(e.r.Context(), e.conn, stream, payload); err != nil {
		e.log.Printf("api: exec: %v", err)
	}
}

// stream is the io.Writer the guest's output is copied into, one message per copy.
func (e *execSession) stream(stream byte) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		if err := Send(e.r.Context(), e.conn, stream, p); err != nil {
			return 0, err
		}

		return len(p), nil
	})
}

// close ends the WebSocket of this attach. A client that left first closed it; the command it streamed runs on.
func (e *execSession) close() {
	e.closeStdin(io.EOF)

	if e.conn == nil {
		return
	}

	err := e.conn.Close(websocket.StatusNormalClosure, "")
	if err != nil && !errors.Is(err, net.ErrClosed) {
		e.log.Printf("api: exec: close the WebSocket: %v", err)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func (h *Handler) resizeExec(w http.ResponseWriter, r *http.Request) {
	var size sandbox.TerminalSize
	if err := decode(r, &size); err != nil {
		h.writeError(w, err)

		return
	}

	if err := h.lifecycle.ResizeExec(r.Context(), r.PathValue("id"), r.PathValue("exec"), size); err != nil {
		h.writeError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) sandboxLogs(w http.ResponseWriter, r *http.Request) {
	follow, err := boolQuery(r, "follow")
	if err != nil {
		h.writeError(w, err)

		return
	}

	if follow {
		h.followLogs(w, r)

		return
	}

	out := &logWriter{w: w, contentType: plainText}

	err = h.lifecycle.Logs(r.Context(), r.PathValue("id"), out)

	// Once a byte is out the status is already 200, so the rest of the failure goes to the daemon's log.
	if err != nil && out.wrote {
		h.log.Printf("api: logs of sandbox %s: %v", r.PathValue("id"), err)

		return
	}
	if err != nil {
		h.writeError(w, err)

		return
	}

	// A sandbox that wrote nothing still answers, and an empty body is what it wrote.
	if !out.wrote {
		if err := out.header(); err != nil {
			h.log.Printf("api: logs of sandbox %s: %v", r.PathValue("id"), err)
		}
	}
}

// The content types of a log body: the output as it was written, and the egress decisions one JSON record per line.
const (
	plainText = "text/plain; charset=utf-8"
	ndjson    = "application/x-ndjson"
)

// logWriter answers 200 on the first byte and flushes every write, so the body arrives as it is read.
type logWriter struct {
	w           http.ResponseWriter
	contentType string
	wrote       bool
}

// header commits the 200 and flushes it, so curl -N sees the response before the first record.
func (l *logWriter) header() error {
	l.w.Header().Set("Content-Type", l.contentType)
	l.w.WriteHeader(http.StatusOK)
	l.wrote = true

	if err := http.NewResponseController(l.w).Flush(); err != nil {
		return fmt.Errorf("flush the header: %w", err)
	}

	return nil
}

func (l *logWriter) Write(p []byte) (int, error) {
	if !l.wrote {
		if err := l.header(); err != nil {
			return 0, err
		}
	}

	n, err := l.w.Write(p)
	if err != nil {
		return n, fmt.Errorf("write the output: %w", err)
	}

	if err := http.NewResponseController(l.w).Flush(); err != nil {
		return n, fmt.Errorf("flush the output: %w", err)
	}

	return n, nil
}

// followLogs streams the output as it comes: over a WebSocket with the handshake, as a chunked body without it.
func (h *Handler) followLogs(w http.ResponseWriter, r *http.Request) {
	// A reference nothing holds is refused before the 101, like every other refusal.
	id, err := h.repo.Resolve(r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)

		return
	}
	if _, err := h.repo.Get(id); err != nil {
		h.writeError(w, err)

		return
	}

	if !isHandshake(r) {
		h.followLogsPlain(w, r, id)

		return
	}

	f, err := h.follow(w, r, "logs of sandbox "+id)
	if err != nil {
		h.writeError(w, err)

		return
	}
	defer f.close(websocket.StatusNormalClosure, "")

	reason, err := h.lifecycle.FollowLogs(f.ctx, id, writerFunc(func(p []byte) (int, error) {
		if err := Send(f.ctx, f.conn, StreamStdout, p); err != nil {
			return 0, err
		}

		return len(p), nil
	}))

	// The client hung up or the daemon is going down, and neither is anything to say on the wire.
	if f.ctx.Err() != nil {
		return
	}
	if err != nil {
		f.send(StreamFailure, failureOf(err))

		return
	}

	f.send(StreamExit, EndMessage{Reason: reason})
}

// followLogsPlain is the follow for curl -N: the bytes as they come, and the body ends when the sandbox stops or is removed.
func (h *Handler) followLogsPlain(w http.ResponseWriter, r *http.Request, id string) {
	out := &logWriter{w: w, contentType: plainText}
	if err := out.header(); err != nil {
		h.log.Printf("api: logs of sandbox %s: %v", id, err)

		return
	}

	_, err := h.lifecycle.FollowLogs(r.Context(), id, out)

	// A client that hung up is the usual end of a follow, and no failure.
	if r.Context().Err() != nil {
		err = nil
	}
	if err != nil {
		h.log.Printf("api: logs of sandbox %s: %v", id, err)
	}
}

// followEgressLog streams one decision per message, or per line without the handshake, until the sandbox stops or is removed.
func (h *Handler) followEgressLog(w http.ResponseWriter, r *http.Request, sb models.Sandbox) {
	if !isHandshake(r) {
		h.followEgressLogPlain(w, r, sb)

		return
	}

	f, err := h.follow(w, r, "egress log of sandbox "+sb.ID)
	if err != nil {
		h.writeError(w, err)

		return
	}

	ctx, done := h.untilStopped(f.ctx, sb.ID)
	defer done()

	err = h.egressLog.Follow(ctx, sb, func(record egress.Record) error {
		line, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("encode an egress record of sandbox %s: %w", sb.ID, err)
		}

		if err := f.conn.Write(ctx, websocket.MessageText, line); err != nil {
			return fmt.Errorf("send an egress record of sandbox %s: %w", sb.ID, err)
		}

		return nil
	})

	err = endOf(ctx, err)

	switch {
	case errors.Is(err, errStopped), errors.Is(err, egress.ErrSandboxGone):
		f.close(websocket.StatusNormalClosure, err.Error())
	case f.ctx.Err() != nil:
		f.close(websocket.StatusNormalClosure, "")
	case err != nil:
		f.close(websocket.StatusInternalError, err.Error())
	default:
		f.close(websocket.StatusNormalClosure, "")
	}
}

// followEgressLogPlain is the follow for curl -N: one JSON record per line, flushed as it lands.
func (h *Handler) followEgressLogPlain(w http.ResponseWriter, r *http.Request, sb models.Sandbox) {
	out := &logWriter{w: w, contentType: ndjson}
	if err := out.header(); err != nil {
		h.log.Printf("api: egress log of sandbox %s: %v", sb.ID, err)

		return
	}

	ctx, done := h.untilStopped(r.Context(), sb.ID)
	defer done()

	err := h.egressLog.Follow(ctx, sb, func(record egress.Record) error {
		line, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("encode an egress record of sandbox %s: %w", sb.ID, err)
		}

		if _, err := out.Write(append(line, '\n')); err != nil {
			return err
		}

		return nil
	})

	err = endOf(ctx, err)

	// A stop, a rm or a client that hung up ends the body, and none of them is a failure.
	if errors.Is(err, errStopped) || errors.Is(err, egress.ErrSandboxGone) || r.Context().Err() != nil {
		err = nil
	}
	if err != nil {
		h.log.Printf("api: egress log of sandbox %s: %v", sb.ID, err)
	}
}

// errStopped ends an egress follow: a stopped sandbox makes no more decisions, so there is nothing left to follow.
var errStopped = errors.New("the sandbox stopped")

// stopPoll is how often an egress follow asks the record whether the sandbox stopped.
const stopPoll = 500 * time.Millisecond

// untilStopped ends the context once the record says stopped or is gone, since the egress log outlives both.
func (h *Handler) untilStopped(parent context.Context, id string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(stopPoll):
			}

			sb, err := h.repo.Get(id)
			switch {
			case errors.Is(err, sandboxstate.ErrNotFound):
				cancel(egress.ErrSandboxGone)
			case err != nil:
				cancel(fmt.Errorf("ask whether sandbox %s stopped: %w", id, err))
			case sb.State == models.StateStopped:
				cancel(errStopped)
			}
		}
	}()

	return ctx, func() { cancel(nil) }
}

// endOf names why an egress follow ended: what the record poll saw wins over the follow's own word.
func endOf(ctx context.Context, err error) error {
	cause := context.Cause(ctx)
	if cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}

	return err
}

// follower is one WebSocket the daemon only writes to; its ctx ends when the client closes or goes away.
type follower struct {
	conn *websocket.Conn
	ctx  context.Context
	log  *log.Logger
	what string
}

// follow answers the 101 and starts reading for the close. Both callers gate the handshake, so it never sees a plain request.
func (h *Handler) follow(w http.ResponseWriter, r *http.Request, what string) (*follower, error) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, fmt.Errorf("open the WebSocket of the %s: %w", what, err)
	}

	return &follower{conn: conn, ctx: conn.CloseRead(r.Context()), log: h.log, what: what}, nil
}

func (f *follower) send(stream byte, payload any) {
	if err := sendJSON(f.ctx, f.conn, stream, payload); err != nil {
		f.log.Printf("api: %s: %v", f.what, err)
	}
}

// close ends the follow with a status and a reason the client can print; a reason has 123 bytes on the wire.
func (f *follower) close(code websocket.StatusCode, reason string) {
	if len(reason) > 123 {
		reason = strings.ToValidUTF8(reason[:123], "")
	}

	err := f.conn.Close(code, reason)
	if err != nil && !errors.Is(err, net.ErrClosed) {
		f.log.Printf("api: %s: close the WebSocket: %v", f.what, err)
	}
}

// sendJSON encodes payload as the message of stream; nothing the daemon sends is over MaxPayload.
func sendJSON(ctx context.Context, conn *websocket.Conn, stream byte, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode the message of stream %d: %w", stream, err)
	}

	return Send(ctx, conn, stream, body)
}

// failureOf is the failure message of an error, the same status and code an error body would carry.
func failureOf(err error) FailureMessage {
	_, code := classify(err)

	return FailureMessage{Error: FailureError{Code: code, Message: err.Error()}}
}
