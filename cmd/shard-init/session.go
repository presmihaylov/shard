package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/supervisor"
)

// acceptExec gives every exec its own connection, so one slow session never blocks the next.
func (t *transport) acceptExec(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: accept an exec connection:", err)

			return
		}
		go t.serveExec(conn)
	}
}

// session is one exec: the frames the host sends and the frames the child's output becomes.
type session struct {
	conn    net.Conn
	writeMu sync.Mutex
	stdin   io.WriteCloser
	// stdinOnce lets the frame reader and the release both close stdin, and only the first one logs a failure.
	stdinOnce sync.Once
	term      *pty.Pty
}

func (t *transport) serveExec(conn net.Conn) {
	defer conn.Close()

	s := &session{conn: conn}
	var header supervisor.ExecHeader
	if err := supervisor.ReadHeader(conn, &header); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: read an exec header:", err)

		return
	}

	exit, err := s.run(t.g, header)
	if err != nil {
		s.finish(supervisor.ExitFrame{Code: startFailureCode(err), Error: err.Error()})

		return
	}
	s.finish(supervisor.ExitFrame{Code: exit.Code, Signal: exit.Signal})
}

// run starts the command over three pipes, or one pty, and pumps both directions until it exits.
func (s *session) run(g *guest, header supervisor.ExecHeader) (models.ExitStatus, error) {
	if len(header.Argv) == 0 {
		return models.ExitStatus{}, errors.New("the exec header has no argv")
	}

	credential, err := execCredential(header)
	if err != nil {
		return models.ExitStatus{}, err
	}

	files, outputs, err := s.open(header)
	if err != nil {
		return models.ExitStatus{}, err
	}

	ep := entrypoint{argv: header.Argv, env: header.Env, dir: header.WorkDir, credential: credential}
	pid, exited, err := g.spawn(ep, files, header.TTY)
	// The child holds its own copies, so the supervisor's ends close whether the start took or not.
	closeAll(files)
	if err != nil {
		s.release(outputs)

		return models.ExitStatus{}, err
	}

	if err := s.send(supervisor.StreamStarted, supervisor.StartedFrame{PID: pid}); err != nil {
		// The host never learned the pid, so nothing else can end the command.
		g.kill(pid)
		<-exited
		s.release(outputs)

		return models.ExitStatus{}, err
	}

	var pumps sync.WaitGroup
	for i, out := range outputs {
		pumps.Add(1)
		go func(stream byte, r *os.File) {
			defer pumps.Done()
			s.pump(stream, r)
		}(supervisor.StreamStdout+byte(i), out)
	}
	go s.readFrames(g, pid)

	exit := <-exited
	// A pty's output ends with EIO once the last holder closes, a pipe with EOF; both end the pump.
	pumps.Wait()
	s.release(outputs)

	return exit, nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// release closes the supervisor's ends: the read ends, the stdin pipe, and the pty behind them when the exec had one.
func (s *session) release(outputs []*os.File) {
	closeAll(outputs)
	s.closeStdin()
	if s.term != nil {
		_ = s.term.Close()
	}
}

// open builds the child's fds: a pty's replica three times, or a stdin pipe and two output pipes.
func (s *session) open(header supervisor.ExecHeader) (files, outputs []*os.File, err error) {
	if header.TTY {
		term, err := pty.Open()
		if err != nil {
			return nil, nil, fmt.Errorf("open a pty: %w", err)
		}
		if header.Rows > 0 || header.Cols > 0 {
			if err := term.Resize(pty.Size{Rows: header.Rows, Cols: header.Cols}); err != nil {
				_ = term.Close()

				return nil, nil, fmt.Errorf("size the pty: %w", err)
			}
		}
		s.term = term
		s.stdin = term.Master
		// The pump owns the master's read side, and dup keeps one close from ending the other.
		out, err := dup(term.Master)
		if err != nil {
			_ = term.Close()

			return nil, nil, err
		}

		return []*os.File{term.Replica, term.Replica, term.Replica}, []*os.File{out}, nil
	}

	var opened []*os.File
	pipe := func(name string) (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll(opened)

			return nil, nil, fmt.Errorf("open the %s pipe: %w", name, err)
		}
		opened = append(opened, r, w)

		return r, w, nil
	}
	stdinR, stdinW, err := pipe("stdin")
	if err != nil {
		return nil, nil, err
	}
	stdoutR, stdoutW, err := pipe("stdout")
	if err != nil {
		return nil, nil, err
	}
	stderrR, stderrW, err := pipe("stderr")
	if err != nil {
		return nil, nil, err
	}
	s.stdin = stdinW

	return []*os.File{stdinR, stdoutW, stderrW}, []*os.File{stdoutR, stderrR}, nil
}

func dup(f *os.File) (*os.File, error) {
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		return nil, fmt.Errorf("dup the pty master: %w", err)
	}

	return os.NewFile(uintptr(fd), f.Name()), nil
}

// pump frames one output until it ends; once a write fails the host is gone, and the rest is read and dropped so the command never blocks on it.
func (s *session) pump(stream byte, r io.Reader) {
	buf := make([]byte, 32<<10)
	hungUp := false
	for {
		n, err := r.Read(buf)
		if n > 0 && !hungUp {
			hungUp = s.write(stream, buf[:n]) != nil
		}
		if err != nil {
			return
		}
	}
}

// readFrames takes stdin, its close, a resize and a cancel from the host; a hang-up with no cancel is a daemon restart, and the command runs on.
func (s *session) readFrames(g *guest, pid int) {
	for {
		stream, payload, err := supervisor.ReadFrame(s.conn)
		if err != nil {
			s.closeStdin()

			return
		}
		switch stream {
		case supervisor.StreamCancel:
			s.closeStdin()
			g.kill(pid)

			return
		case supervisor.StreamStdin:
			if _, err := s.stdin.Write(payload); err != nil {
				s.closeStdin()
			}
		case supervisor.StreamStdinClose:
			s.closeStdin()
		case supervisor.StreamResize:
			s.resize(payload)
		default:
			fmt.Fprintf(os.Stderr, "shard-init: exec %d: stream %d is not one the guest takes\n", pid, stream)
		}
	}
}

// closeStdin ends the child's input once; on a pty the master stays open, since it is also the output.
func (s *session) closeStdin() {
	if s.term != nil {
		return
	}
	s.stdinOnce.Do(func() {
		// Pres ruled 2026-10-03 (SHARD-354): log and continue, since no caller waits on a stdin close and a failed close leaves nothing to retry.
		if err := s.stdin.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: close the stdin of an exec:", err)
		}
	})
}

func (s *session) resize(payload []byte) {
	if s.term == nil {
		return
	}
	var size supervisor.ResizeFrame
	if err := supervisor.DecodeFrame(payload, &size); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: resize:", err)

		return
	}
	if err := s.term.Resize(pty.Size{Rows: size.Rows, Cols: size.Cols}); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: resize:", err)
	}
}

func (s *session) write(stream byte, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	return supervisor.WriteFrame(s.conn, stream, payload)
}

func (s *session) send(stream byte, value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	return supervisor.WriteJSONFrame(s.conn, stream, value)
}

func (s *session) finish(exit supervisor.ExitFrame) {
	if err := s.send(supervisor.StreamExit, exit); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: report an exec exit:", err)
	}
}

// startFailureCode is the shell's convention, which runsc exec reports the same way: 127 missing, 126 not runnable.
func startFailureCode(err error) int {
	// docker exec answers 126 for a work directory it cannot enter, whatever the errno.
	if _, ok := errors.AsType[*workDirError](err); ok {
		return 126
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
		return 127
	}

	return 126
}
