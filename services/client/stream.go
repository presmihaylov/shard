package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

// stdinChunk is how much of the keyboard one frame carries.
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

// Exec runs one command in a sandbox over a connection it takes over from HTTP: the daemon answers
// 101 and both sides then speak frames. It reports the guest's exit status, and a command that never
// ran as a models.CommandNotStartedError.
func (c *Client) Exec(ctx context.Context, ref string, req sandbox.ExecRequest, streams ExecStreams) (exit models.ExitStatus, err error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return models.ExitStatus{}, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()

	stop := interrupt(ctx, conn)
	defer func() { err = errors.Join(err, stop()) }()

	// The daemon gives the command no stdin unless this client has one to type into it.
	req.Stdin = streams.Stdin != nil

	reader, execID, err := c.upgrade(ctx, conn, ref, req)
	if err != nil {
		return models.ExitStatus{}, err
	}
	if streams.Started != nil {
		streams.Started(execID)
	}

	frames := api.NewFrameWriter(conn)
	// Nothing waits for this copier: it blocks on a terminal this process does not own.
	go sendInput(frames, streams)

	return readExec(reader, ref, streams)
}

// upgrade writes the request itself and reads the 101, because net/http gives no connection back.
func (c *Client) upgrade(ctx context.Context, conn net.Conn, ref string, req sandbox.ExecRequest) (*bufio.Reader, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, "", fmt.Errorf("encode the exec of sandbox %s: %w", ref, err)
	}

	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/exec"

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://shard"+path, bytes.NewReader(body)) //nolint:gosec // G704: the ref only lands in the path; this connection is the socket whatever the URL says
	if err != nil {
		return nil, "", fmt.Errorf("build the request for the exec of sandbox %s: %w", ref, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "tcp")

	if err := request.Write(conn); err != nil {
		return nil, "", fmt.Errorf("ask for the exec of sandbox %s on %s: %w", ref, c.path, err)
	}

	reader := bufio.NewReader(conn)

	resp, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, "", fmt.Errorf("read the answer to the exec of sandbox %s on %s: %w", ref, c.path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		answer, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", fmt.Errorf("read the refusal of the exec of sandbox %s: %w", ref, err)
		}

		return nil, "", missing(ref, decodeError(resp.StatusCode, answer))
	}

	return reader, resp.Header.Get(api.ExecIDHeader), nil
}

// sendInput forwards the keyboard and then says so, because a guest that reads waits for the end of it.
func sendInput(frames *api.FrameWriter, streams ExecStreams) {
	if streams.Stdin != nil {
		if err := copyInput(frames, streams.Stdin); err != nil {
			warn(streams.Warn, fmt.Sprintf("the keyboard stopped reaching the command: %v", err))

			return
		}
	}

	if err := frames.Write(api.StreamStdinClose, nil); err != nil {
		warn(streams.Warn, fmt.Sprintf("the command was not told the input had ended: %v", err))
	}
}

func copyInput(frames *api.FrameWriter, r io.Reader) error {
	buf := make([]byte, stdinChunk)

	for {
		n, err := r.Read(buf)
		if n > 0 {
			if err := frames.Write(api.StreamStdin, buf[:n]); err != nil {
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

// readExec writes the guest's output where it belongs and ends on the exit frame, which every exec has.
func readExec(r io.Reader, ref string, streams ExecStreams) (models.ExitStatus, error) {
	var failure string

	for {
		stream, payload, err := api.ReadFrame(r)
		if errors.Is(err, io.EOF) {
			if failure != "" {
				return models.ExitStatus{}, errors.New(failure)
			}

			return models.ExitStatus{}, fmt.Errorf("the exec in sandbox %s ended without an exit status", ref)
		}
		if err != nil {
			return models.ExitStatus{}, err
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
		case api.StreamError:
			failure = string(payload)
		case api.StreamExit:
			return exitOf(payload, ref, failure)
		default:
			return models.ExitStatus{}, fmt.Errorf("the daemon sent a frame of stream %d, which no daemon sends", stream)
		}
	}
}

// exitOf reads the exit frame. An exit that follows a failure is a command the sandbox never ran,
// and the code is then the one a shell answers for the same refusal.
func exitOf(payload []byte, ref, failure string) (models.ExitStatus, error) {
	code, err := strconv.Atoi(string(payload))
	if err != nil {
		return models.ExitStatus{}, fmt.Errorf("the daemon answered %q as the exit status of the exec in sandbox %s", payload, ref)
	}

	if failure != "" {
		return models.ExitStatus{}, &models.CommandNotStartedError{Sandbox: ref, Reason: failure, Code: code}
	}

	return models.ExitStatus{Code: code}, nil
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
// sandbox stops, or when the caller's context does.
func (c *Client) Logs(ctx context.Context, ref string, follow bool, w io.Writer) error {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/logs"
	if follow {
		path += "?follow=true"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://shard"+path, nil) //nolint:gosec // G704: the ref only lands in the path; the dialer goes to the socket whatever the URL says
	if err != nil {
		return fmt.Errorf("build the request for the output of sandbox %s: %w", ref, err)
	}

	resp, err := c.http.Do(req) //nolint:gosec // G704: the ref only lands in the path; the dialer goes to the socket whatever the URL says

	var connect *ConnectError
	if errors.As(err, &connect) {
		return connect
	}
	// An interrupt is how an operator leaves a follow, and it leaves nothing behind on the host.
	if err != nil && ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("GET %s on %s: %w", path, c.path, err)
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
		// An interrupt is how an operator leaves a follow, and it leaves nothing behind on the host.
		if ctx.Err() != nil {
			return nil
		}

		return fmt.Errorf("read the output of sandbox %s: %w", ref, err)
	}

	return nil
}

// interrupt ends the reads when ctx does, so a command the operator gave up on gives the terminal back.
func interrupt(ctx context.Context, conn net.Conn) func() error {
	done := make(chan struct{})
	exited := make(chan error, 1)

	go func() {
		select {
		case <-ctx.Done():
			exited <- conn.SetDeadline(time.Now())
		case <-done:
			exited <- nil
		}
	}()

	return func() error {
		close(done)

		return <-exited
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
