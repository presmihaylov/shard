package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// upgrade is the answer to an exec: the connection stops being HTTP and carries frames both ways.
const upgrade = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: tcp\r\nConnection: Upgrade\r\n"

func (h *Handler) execSandbox(w http.ResponseWriter, r *http.Request) {
	var req sandbox.ExecRequest
	if err := decode(r, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	// A client that hangs up ends the command it was running, and nothing else: only stop ends a sandbox.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	session := &execSession{w: w, log: h.log, cancel: cancel}
	defer session.close()

	stdin, writer := io.Pipe()
	session.stdin = writer

	streams := sandbox.Streams{
		Stdout:  session.stream(StreamStdout),
		Stderr:  session.stream(StreamStderr),
		Started: session.start,
		Warn:    func(message string) { h.log.Printf("api: exec in sandbox %s: %s", r.PathValue("id"), message) },
	}
	// A client that types nothing leaves the command with no stdin, which is what exec without -i means.
	if req.Stdin {
		streams.Stdin = stdin
	}

	exit, err := h.lifecycle.Exec(ctx, r.PathValue("id"), req, streams)

	// Nothing was said on the wire yet, so the refusal is a status and a JSON body like every other route.
	if !session.upgraded {
		h.writeError(w, status(err), err.Error())

		return
	}

	session.finish(exit, err)
}

// execSession is the client side of one exec: the frames it sends in, and the frames the guest sends back.
type execSession struct {
	w      http.ResponseWriter
	log    *log.Logger
	cancel context.CancelFunc

	upgraded bool
	conn     net.Conn
	frames   *FrameWriter
	stdin    *io.PipeWriter
}

// start answers the 101 and takes the connection, so everything after this is frames and never HTTP.
func (e *execSession) start(execID string) error {
	conn, buffered, err := http.NewResponseController(e.w).Hijack()
	if err != nil {
		return fmt.Errorf("take over the connection of exec %s: %w", execID, err)
	}
	e.upgraded, e.conn, e.frames = true, conn, NewFrameWriter(conn)

	if _, err := buffered.WriteString(upgrade + ExecIDHeader + ": " + execID + "\r\n\r\n"); err != nil {
		return fmt.Errorf("answer the exec %s: %w", execID, err)
	}
	if err := buffered.Flush(); err != nil {
		return fmt.Errorf("answer the exec %s: %w", execID, err)
	}

	// The buffered reader may already hold the first frames, so the loop reads from it and not from the connection.
	go e.read(buffered.Reader)

	return nil
}

// read moves the client's frames into the guest's stdin until the client says it is done, or goes away.
func (e *execSession) read(r *bufio.Reader) {
	for {
		stream, payload, err := ReadFrame(r)
		if err != nil {
			// The client is gone, so the command goes with it: an exec belongs to the connection that asked for it.
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
			e.log.Printf("api: exec: the client sent a frame of stream %d, which no client sends", stream)
			e.closeStdin(fmt.Errorf("the client sent a frame of stream %d", stream))
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

// finish says how the command ended. A command that never ran carries both frames, so the client can
// answer with the code a shell answers for it.
func (e *execSession) finish(exit models.ExitStatus, err error) {
	var notStarted *models.CommandNotStartedError
	if errors.As(err, &notStarted) {
		// The reason travels alone, because the client names the sandbox again when it rebuilds the error.
		e.send(StreamError, notStarted.Reason)
		e.send(StreamExit, strconv.Itoa(notStarted.Code))

		return
	}

	if err != nil {
		e.send(StreamError, err.Error())

		return
	}

	e.send(StreamExit, strconv.Itoa(exit.Code))
}

func (e *execSession) send(stream byte, payload string) {
	if err := e.frames.Write(stream, []byte(payload)); err != nil {
		e.log.Printf("api: exec: write the frame of stream %d: %v", stream, err)
	}
}

// stream is the io.Writer the guest's output is copied into, one frame per copy.
func (e *execSession) stream(stream byte) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		if err := e.frames.Write(stream, p); err != nil {
			return 0, err
		}

		return len(p), nil
	})
}

// close ends the connection this exec owned. A session that never upgraded still owns the response.
func (e *execSession) close() {
	e.closeStdin(io.EOF)

	if !e.upgraded {
		return
	}

	if err := e.conn.Close(); err != nil {
		e.log.Printf("api: exec: close the connection: %v", err)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func (h *Handler) resizeExec(w http.ResponseWriter, r *http.Request) {
	var size sandbox.TerminalSize
	if err := decode(r, &size); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	if err := h.lifecycle.ResizeExec(r.Context(), r.PathValue("id"), r.PathValue("exec"), size); err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) sandboxLogs(w http.ResponseWriter, r *http.Request) {
	follow, err := boolQuery(r, "follow")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	out := &logWriter{w: w}

	err = h.lifecycle.Logs(r.Context(), r.PathValue("id"), follow, out)

	// Once a byte is out the status is already 200, so the rest of the failure goes to the daemon's log.
	if err != nil && out.wrote {
		h.log.Printf("api: logs of sandbox %s: %v", r.PathValue("id"), err)

		return
	}
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	// A sandbox that wrote nothing still answers, and an empty body is what it wrote.
	if !out.wrote {
		out.header()
	}
}

// logWriter answers 200 on the first byte and flushes every write, so a follow arrives as it happens.
type logWriter struct {
	w     http.ResponseWriter
	wrote bool
}

func (l *logWriter) header() {
	l.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	l.w.WriteHeader(http.StatusOK)
	l.wrote = true
}

func (l *logWriter) Write(p []byte) (int, error) {
	if !l.wrote {
		l.header()
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
