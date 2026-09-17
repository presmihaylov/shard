package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"

	"github.com/coder/websocket"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

// stdinChunk is how much of the keyboard one message carries.
const stdinChunk = 32 * 1024

// ExecStreams is where one exec's stdio goes on this side of the socket.
type ExecStreams struct {
	// Stdin is nil for a command that reads nothing, which the daemon is told at once.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Started is called with the exec id the daemon named, which is what a resize sends.
	Started func(execID string)
	// Warn reports what the keyboard copier cannot return, because nothing waits for that goroutine.
	Warn func(message string)
}

// execsResult is a page of one sandbox's execs. A list with no limit is the whole set in one page.
type execsResult struct {
	Execs []models.Exec `json:"execs"`
}

// Exec starts the command, then attaches over a WebSocket; a command that never ran is a CommandNotStartedError.
func (c *Client) Exec(ctx context.Context, ref string, req sandbox.ExecRequest, streams ExecStreams) (models.ExitStatus, error) {
	// The daemon gives the command no stdin unless this client has one to type into it.
	req.Stdin = streams.Stdin != nil

	exec, err := c.CreateExec(ctx, ref, req)
	if err != nil {
		return models.ExitStatus{}, err
	}

	return c.AttachExec(ctx, ref, exec.ID, streams)
}

// CreateExec starts one command at once and answers the record the daemon named for it.
func (c *Client) CreateExec(ctx context.Context, ref string, req sandbox.ExecRequest) (models.Exec, error) {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/exec"

	var exec models.Exec
	if err := c.call(ctx, http.MethodPost, path, req, &exec, c.Timeout); err != nil {
		return models.Exec{}, missing(ref, err)
	}

	return exec, nil
}

// AttachExec replays the exec's buffer, then streams it live until the command ends. A dropped attach
// leaves the command running: only stop ends a sandbox, and a kill ends one command.
func (c *Client) AttachExec(ctx context.Context, ref, execID string, streams ExecStreams) (exit models.ExitStatus, err error) {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/exec/" + url.PathEscape(execID)

	conn, err := c.open(ctx, path, "the exec of sandbox "+ref)
	if err != nil {
		return models.ExitStatus{}, missing(ref, err)
	}
	defer func() { err = errors.Join(err, closeStream(conn)) }()

	if streams.Started != nil {
		streams.Started(execID)
	}

	// Nothing waits for this copier: it blocks on a terminal this process does not own.
	go sendInput(ctx, conn, streams)

	return readExec(ctx, conn, ref, streams)
}

// ListExecs answers every exec the sandbox holds, oldest id first.
func (c *Client) ListExecs(ctx context.Context, ref string) ([]models.Exec, error) {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/exec"

	var out execsResult
	if err := c.call(ctx, http.MethodGet, path, nil, &out, c.Timeout); err != nil {
		return nil, missing(ref, err)
	}

	return out.Execs, nil
}

// GetExec answers the exec's record as it stands now.
func (c *Client) GetExec(ctx context.Context, ref, execID string) (models.Exec, error) {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/exec/" + url.PathEscape(execID)

	var exec models.Exec
	if err := c.call(ctx, http.MethodGet, path, nil, &exec, c.Timeout); err != nil {
		return models.Exec{}, missing(ref, err)
	}

	return exec, nil
}

// WaitExec blocks until the exec ends, then answers its record. A wait has no bound of its own.
func (c *Client) WaitExec(ctx context.Context, ref, execID string) (models.Exec, error) {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/exec/" + url.PathEscape(execID) + "?wait=true"

	var exec models.Exec
	if err := c.call(ctx, http.MethodGet, path, nil, &exec, 0); err != nil {
		return models.Exec{}, missing(ref, err)
	}

	return exec, nil
}

// KillExec sends one signal to a running exec. An empty signal is TERM.
func (c *Client) KillExec(ctx context.Context, ref, execID, signal string) error {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/exec/" + url.PathEscape(execID) + "/kill"

	req := struct {
		Signal string `json:"signal,omitempty"`
	}{Signal: signal}

	if err := c.call(ctx, http.MethodPost, path, req, nil, c.Timeout); err != nil {
		return missing(ref, err)
	}

	return nil
}

// DeleteExec forgets an exec that has ended and frees its buffer.
func (c *Client) DeleteExec(ctx context.Context, ref, execID string) error {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/exec/" + url.PathEscape(execID)

	if err := c.call(ctx, http.MethodDelete, path, nil, nil, c.Timeout); err != nil {
		return missing(ref, err)
	}

	return nil
}

// open dials one streaming route. A refusal comes before the 101, as the status and the body any call gets.
func (c *Client) open(ctx context.Context, path, what string) (*websocket.Conn, error) {
	options := &websocket.DialOptions{HTTPClient: c.http, HTTPHeader: http.Header{}}
	c.authorize(options.HTTPHeader)

	conn, resp, err := websocket.Dial(ctx, "ws://shard"+path, options) //nolint:gosec // G704: the ref only lands in the path; the dialer goes to the socket whatever the URL says
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	var connect *ConnectError
	if errors.As(err, &connect) {
		return nil, connect
	}
	if err != nil && resp != nil {
		answer, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("read the refusal of %s: %w", what, readErr)
		}

		return nil, decodeError(resp.StatusCode, answer)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s on %s: %w", what, c.target, err)
	}

	conn.SetReadLimit(api.MaxPayload + 1)

	return conn, nil
}

// closeStream ends a session both sides are done with; one the library closed on a cancelled context reports nothing.
func closeStream(conn *websocket.Conn) error {
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("close the stream: %w", err)
	}

	return nil
}

// sendInput forwards the keyboard and then says so, because a guest that reads waits for the end of it.
func sendInput(ctx context.Context, conn *websocket.Conn, streams ExecStreams) {
	if streams.Stdin != nil {
		if err := copyInput(ctx, conn, streams.Stdin); err != nil {
			if !gone(ctx, err) {
				warn(streams.Warn, fmt.Sprintf("the keyboard stopped reaching the command: %v", err))
			}

			return
		}
	}

	// A command that exited first took the session with it, and its exit message already said so.
	if err := api.Send(ctx, conn, api.StreamStdinClose, nil); err != nil && !gone(ctx, err) {
		warn(streams.Warn, fmt.Sprintf("the command was not told the input had ended: %v", err))
	}
}

// gone reports the errors a write hits once the other end of the session is done with it.
func gone(ctx context.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}

func copyInput(ctx context.Context, conn *websocket.Conn, r io.Reader) error {
	buf := make([]byte, stdinChunk)

	for {
		n, err := r.Read(buf)
		if n > 0 {
			if err := api.Send(ctx, conn, api.StreamStdin, buf[:n]); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readExec writes the guest's output where it belongs and ends on the exit or the failure, which every exec has.
func readExec(ctx context.Context, conn *websocket.Conn, ref string, streams ExecStreams) (models.ExitStatus, error) {
	for {
		stream, payload, err := api.Receive(ctx, conn)
		if err != nil {
			return models.ExitStatus{}, ended(ctx, err, ref)
		}

		switch stream {
		case api.StreamStdout:
			if err := write(streams.Stdout, payload); err != nil {
				return models.ExitStatus{}, err
			}
		case api.StreamStderr:
			if err := write(streams.Stderr, payload); err != nil {
				return models.ExitStatus{}, err
			}
		case api.StreamExit:
			return exitOf(payload, ref)
		case api.StreamFailure:
			return models.ExitStatus{}, failureOf(payload, ref)
		default:
			return models.ExitStatus{}, fmt.Errorf("the daemon sent a message of stream %d, which no daemon sends", stream)
		}
	}
}

// ended names a session that ended before its exit, which only an interrupt on this side does on purpose.
func ended(ctx context.Context, err error, ref string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("the exec in sandbox %s: %w", ref, ctx.Err())
	}

	return fmt.Errorf("the exec in sandbox %s ended without an exit status: %w", ref, err)
}

// exitOf reads the exit message; one that carries an error is a command the sandbox never ran, with a shell's code.
func exitOf(payload []byte, ref string) (models.ExitStatus, error) {
	var exit api.ExitMessage
	if err := json.Unmarshal(payload, &exit); err != nil {
		return models.ExitStatus{}, fmt.Errorf("the daemon answered %q as the exit status of the exec in sandbox %s", payload, ref)
	}

	if exit.Error != "" {
		return models.ExitStatus{}, &models.CommandNotStartedError{Sandbox: ref, Reason: exit.Error, Code: exit.Code}
	}

	return models.ExitStatus{Code: exit.Code, Signal: exit.Signal}, nil
}

// failureOf reads a failure message into the error a refusal before the 101 would have been.
func failureOf(payload []byte, ref string) error {
	var failure api.FailureMessage
	if err := json.Unmarshal(payload, &failure); err != nil || failure.Error.Message == "" {
		return fmt.Errorf("the daemon answered %q as the failure of the exec in sandbox %s", payload, ref)
	}

	return &APIError{Code: failure.Error.Code, Message: failure.Error.Message}
}

// ResizeExec sets the window of a running exec, which is what this terminal's SIGWINCH forwards.
func (c *Client) ResizeExec(ctx context.Context, ref, execID string, size sandbox.TerminalSize) error {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/exec/" + url.PathEscape(execID) + "/resize"

	if err := c.call(ctx, http.MethodPost, path, size, nil, c.Timeout); err != nil {
		return missing(ref, err)
	}

	return nil
}

// Logs writes what the entrypoint wrote into w. A follow has no bound of its own: it ends when the
// sandbox stops or is removed, or when the caller's context does.
func (c *Client) Logs(ctx context.Context, ref string, follow bool, w io.Writer) error {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/logs"
	if follow {
		return c.followLogs(ctx, ref, path+"?follow=true", w)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://shard"+path, nil) //nolint:gosec // G704: the ref only lands in the path; the dialer goes to the socket whatever the URL says
	if err != nil {
		return fmt.Errorf("build the request for the output of sandbox %s: %w", ref, err)
	}
	c.authorize(req.Header)

	resp, err := c.http.Do(req) //nolint:gosec // G704: the ref only lands in the path; the dialer goes to the socket whatever the URL says

	var connect *ConnectError
	if errors.As(err, &connect) {
		return connect
	}
	if err != nil {
		return fmt.Errorf("GET %s on %s: %w", path, c.target, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		answer, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("read the refusal of the output of sandbox %s: %w", ref, err)
		}

		return missing(ref, decodeError(resp.StatusCode, answer))
	}

	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("read the output of sandbox %s: %w", ref, err)
	}

	return nil
}

// followLogs prints the output as the daemon sends it and ends on the end message, whatever its reason.
func (c *Client) followLogs(ctx context.Context, ref, path string, w io.Writer) (err error) {
	conn, err := c.open(ctx, path, "the output of sandbox "+ref)
	if err != nil && ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return missing(ref, err)
	}
	defer func() { err = errors.Join(err, closeStream(conn)) }()

	for {
		stream, payload, err := api.Receive(ctx, conn)
		// An interrupt is how an operator leaves a follow, and it leaves nothing behind on the host.
		if err != nil && ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("follow the output of sandbox %s: %w", ref, err)
		}

		switch stream {
		case api.StreamStdout:
			if err := write(w, payload); err != nil {
				return err
			}
		case api.StreamExit:
			return nil
		case api.StreamFailure:
			return failureOf(payload, ref)
		default:
			return fmt.Errorf("the daemon sent a message of stream %d, which no daemon sends", stream)
		}
	}
}

func write(w io.Writer, payload []byte) error {
	if w == nil {
		return nil
	}

	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

func warn(report func(string), message string) {
	if report == nil {
		return
	}

	report(message)
}

// FollowEgressLog prints one decision per line as the daemon writes it, until the caller's context
// ends. A sandbox removed under the follow ends it with a word on why, and never a bare close.
func (c *Client) FollowEgressLog(ctx context.Context, ref string, out, errOut io.Writer) (err error) {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/egress-log?follow=true"

	conn, err := c.open(ctx, path, "the egress log of sandbox "+ref)
	if err != nil && ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return missing(ref, err)
	}
	defer func() { err = errors.Join(err, closeStream(conn)) }()

	for {
		kind, record, err := conn.Read(ctx)
		// An interrupt is how an operator leaves a follow, and it leaves nothing behind on the host.
		if err != nil && ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return egressLogEnd(err, ref, errOut)
		}
		if kind != websocket.MessageText {
			return fmt.Errorf("the daemon sent a binary message on the egress log of sandbox %s, which no daemon sends", ref)
		}

		if err := write(out, append(record, '\n')); err != nil {
			return err
		}
	}
}

// egressLogEnd reads the daemon's close: a normal one with a reason is why the follow ended, and not a failure of it.
func egressLogEnd(err error, ref string, errOut io.Writer) error {
	var closed websocket.CloseError
	if !errors.As(err, &closed) {
		return fmt.Errorf("follow the egress log of sandbox %s: %w", ref, err)
	}

	if closed.Code != websocket.StatusNormalClosure {
		return fmt.Errorf("the egress log of sandbox %s ended: %s", ref, closed.Reason)
	}
	if closed.Reason == "" {
		return nil
	}

	reason := fmt.Sprintf("the egress log of sandbox %s ended: %s\n", ref, closed.Reason)
	if err := write(errOut, []byte(reason)); err != nil {
		return fmt.Errorf("write why the egress log of sandbox %s ended: %w", ref, err)
	}

	return nil
}
