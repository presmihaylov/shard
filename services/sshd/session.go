package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/supervisor"
)

const (
	// refusedExitCode is what ssh itself exits with when it could not run the command.
	refusedExitCode = 255
	// maxEnv is the env requests OpenSSH's sshd takes in one session.
	maxEnv = 128
	// cleanupBudget bounds the kill and the delete of an exec its session left, which outlive the client.
	cleanupBudget = 10 * time.Second
)

// signalNames is the RFC 4254 name of each Linux signal an exit-signal can carry.
var signalNames = map[int]string{
	1: "HUP", 2: "INT", 3: "QUIT", 4: "ILL", 6: "ABRT", 8: "FPE", 9: "KILL",
	10: "USR1", 11: "SEGV", 12: "USR2", 13: "PIPE", 14: "ALRM", 15: "TERM",
}

// session is one session channel: the requests that shape its command, then that command as one exec.
type session struct {
	conn   *connection
	ch     ssh.Channel
	ctx    context.Context
	cancel context.CancelFunc
	// work is the command and the feed of its stdin, which both end once the channel does.
	work sync.WaitGroup

	// Only the request loop touches these, and only before the command starts.
	started bool
	tty     bool
	term    string
	env     []string

	mu     sync.Mutex
	size   sandbox.TerminalSize
	execID string

	hungUp sync.Once
}

// ending is how a command's channel closes: with its exit status, or with the signal that ended it.
type ending struct {
	code   int
	signal string
}

func (c *connection) serveSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	ctx, cancel := context.WithCancel(c.ctx)
	s := &session{conn: c, ch: ch, ctx: ctx, cancel: cancel}

	for req := range reqs {
		ok, run := s.request(req)
		if err := req.Reply(ok, nil); err != nil && !gone(err) {
			s.report("answer a %s request", err)
		}
		if run != nil {
			s.work.Go(run)
		}
	}
	// The channel closed, so nothing reads the command's output; the cancel ends its attach and leaves it to the cleanup.
	cancel()
	s.work.Wait()
}

func (s *session) request(req *ssh.Request) (bool, func()) {
	switch req.Type {
	case "pty-req":
		return s.ptyReq(req.Payload), nil
	case "env":
		return s.setEnv(req.Payload), nil
	case "window-change":
		return s.windowChange(req.Payload), nil
	case "signal":
		return s.signal(req.Payload), nil
	case "shell":
		return s.start([]string{"sh", "-c", sandbox.ShellScript}, s.tty)
	case "exec":
		var msg struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
			return false, nil
		}

		return s.start([]string{"sh", "-c", msg.Command}, s.tty)
	case "subsystem":
		var msg struct{ Name string }
		if err := ssh.Unmarshal(req.Payload, &msg); err != nil || msg.Name != "sftp" {
			return false, nil
		}

		// A terminal would mangle the binary protocol, whatever the client asked for.
		return s.start([]string{supervisor.InitPath, supervisor.SFTPMode}, false)
	}

	// x11-req, auth-agent-req@openssh.com and the rest reach nothing in a sandbox.
	return false, nil
}

func (s *session) start(argv []string, tty bool) (bool, func()) {
	if s.started {
		return false, nil
	}
	s.started = true

	return true, func() { s.run(argv, tty) }
}

func (s *session) ptyReq(payload []byte) bool {
	var msg struct {
		Term          string
		Columns, Rows uint32
		Width, Height uint32
		Modes         string
	}
	if s.started || ssh.Unmarshal(payload, &msg) != nil {
		return false
	}

	s.tty = true
	s.term = msg.Term
	s.mu.Lock()
	s.size = sandbox.TerminalSize{Rows: dimension(msg.Rows), Cols: dimension(msg.Columns)}
	s.mu.Unlock()

	return true
}

func (s *session) setEnv(payload []byte) bool {
	var msg struct{ Name, Value string }
	if s.started || len(s.env) >= maxEnv || ssh.Unmarshal(payload, &msg) != nil {
		return false
	}
	if msg.Name == "" || strings.ContainsAny(msg.Name, "=\x00") || strings.ContainsRune(msg.Value, 0) {
		return false
	}

	s.env = append(s.env, msg.Name+"="+msg.Value)

	return true
}

func (s *session) windowChange(payload []byte) bool {
	var msg struct {
		Columns, Rows uint32
		Width, Height uint32
	}
	if ssh.Unmarshal(payload, &msg) != nil {
		return false
	}

	size := sandbox.TerminalSize{Rows: dimension(msg.Rows), Cols: dimension(msg.Columns)}
	s.mu.Lock()
	s.size = size
	execID := s.execID
	s.mu.Unlock()
	// A change before the exec exists rides on its create instead.
	if execID == "" {
		return true
	}

	return s.conn.srv.sandboxes.ResizeExec(s.ctx, s.conn.opts.Sandbox, execID, size) == nil
}

func (s *session) signal(payload []byte) bool {
	var msg struct{ Signal string }
	// An empty name would reach KillExec as its TERM default, which the client did not ask for.
	if ssh.Unmarshal(payload, &msg) != nil || msg.Signal == "" {
		return false
	}

	s.mu.Lock()
	execID := s.execID
	s.mu.Unlock()
	if execID == "" {
		return false
	}

	return s.conn.srv.sandboxes.KillExec(s.ctx, s.conn.opts.Sandbox, execID, msg.Signal) == nil
}

// run is the command's whole life on the channel: the exec, its exit, and the close.
func (s *session) run(argv []string, tty bool) {
	end := s.exec(argv, tty)
	// The client left, so there is no channel to tell how the command ended.
	if s.ctx.Err() != nil {
		s.hangUp()

		return
	}

	s.finish(end)
}

func (s *session) exec(argv []string, tty bool) ending {
	sandboxes, ref := s.conn.srv.sandboxes, s.conn.opts.Sandbox

	s.mu.Lock()
	size := s.size
	s.mu.Unlock()
	req := sandbox.ExecRequest{Command: argv, Env: s.environment(tty), User: s.conn.user, Stdin: true, TTY: tty, Size: size, Attach: true}
	created, err := sandboxes.CreateExec(s.ctx, ref, req)
	if err != nil {
		return s.refused(err, tty)
	}
	defer s.cleanup(created.ID)
	s.track(created.ID, size)

	in, feed := io.Pipe()
	// An attach refused before it takes stdin never stops it, and feed would wait in a write forever.
	defer func() {
		if err := in.Close(); err != nil {
			s.report("close the command's input", err)
		}
	}()
	s.work.Go(func() { s.feed(feed) })

	streams := sandbox.Streams{Stdin: in, StopStdin: in.Close, Stdout: s.ch, Stderr: s.ch.Stderr(), Detach: s.hangUp}
	attached, err := sandboxes.Attach(s.ctx, ref, created.ID, streams)
	if err != nil {
		return s.refused(err, tty)
	}

	return endingOf(attached.Exit)
}

func (s *session) environment(tty bool) []string {
	named := slices.ContainsFunc(s.env, func(entry string) bool { return strings.HasPrefix(entry, "TERM=") })
	if !tty || s.term == "" || named {
		return s.env
	}

	return append(slices.Clip(s.env), "TERM="+s.term)
}

// track names the exec to the resize and signal requests, and passes on a resize that came during the create.
func (s *session) track(execID string, created sandbox.TerminalSize) {
	s.mu.Lock()
	s.execID = execID
	size := s.size
	s.mu.Unlock()
	if size == created {
		return
	}

	if err := s.conn.srv.sandboxes.ResizeExec(s.ctx, s.conn.opts.Sandbox, execID, size); err != nil && s.ctx.Err() == nil {
		s.report("resize exec "+execID, err)
	}
}

// feed copies what the client types into the command, and closes its stdin at the client's EOF.
func (s *session) feed(w *io.PipeWriter) {
	_, err := io.Copy(w, s.ch)
	// The attach closed its end because the command ended, so nothing was owed.
	if errors.Is(err, io.ErrClosedPipe) {
		err = nil
	}
	if err := errors.Join(err, w.CloseWithError(err)); err != nil && !gone(err) {
		s.report("copy the client's input", err)
	}
}

// refused tells the client on stderr why its command did not run, unless the client is gone already.
func (s *session) refused(err error, tty bool) ending {
	if s.ctx.Err() != nil {
		return ending{}
	}

	newline := "\n"
	if tty {
		newline = "\r\n"
	}
	if _, werr := io.WriteString(s.ch.Stderr(), s.conn.opts.Explain(err)+newline); werr != nil && !gone(werr) {
		s.report("tell the client its command did not run", werr)
	}

	if notStarted, ok := errors.AsType[*models.CommandNotStartedError](err); ok {
		return ending{code: notStarted.Code}
	}

	return ending{code: refusedExitCode}
}

// cleanup ends the exec if the client left it running, then forgets it, since an ssh session owns its command.
func (s *session) cleanup(execID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), cleanupBudget)
	defer cancel()
	sandboxes, ref := s.conn.srv.sandboxes, s.conn.opts.Sandbox

	err := sandboxes.KillExec(ctx, ref, execID, "KILL")
	if _, exited := errors.AsType[*sandbox.ExecExitedError](err); exited {
		err = nil
	}
	if err == nil {
		_, err = sandboxes.WaitExec(ctx, ref, execID)
	}
	if err == nil {
		err = sandboxes.DeleteExec(ctx, ref, execID)
	}
	// A stop or a remove of the sandbox took its execs with it.
	if _, forgotten := errors.AsType[*models.NotFoundError](err); forgotten || err == nil {
		return
	}

	s.report("end exec "+execID, err)
}

// finish closes the channel the way sshd does: end of output, then the exit, then the close.
func (s *session) finish(end ending) {
	exit := "exit-status"
	payload := ssh.Marshal(struct{ Status uint32 }{Status: exitCode(end.code)})
	if end.signal != "" {
		exit = "exit-signal"
		payload = ssh.Marshal(struct {
			Signal     string
			CoreDumped bool
			Error      string
			Lang       string
		}{Signal: end.signal})
	}

	eof := s.ch.CloseWrite()
	_, sent := s.ch.SendRequest(exit, false, payload)
	if err := errors.Join(eof, sent); err != nil && !gone(err) {
		s.report("send the exit", err)
	}
	s.hangUp()
}

// hangUp closes the channel once, which also ends a write blocked on a client that stopped reading.
func (s *session) hangUp() {
	s.hungUp.Do(func() {
		if err := s.ch.Close(); err != nil && !gone(err) {
			s.report("close the session", err)
		}
	})
}

func (s *session) report(what string, err error) {
	s.conn.srv.report(fmt.Sprintf("ssh: sandbox %s: %s: %v", s.conn.opts.Sandbox, what, err))
}

func endingOf(exit models.ExitStatus) ending {
	if exit.Signal == 0 {
		return ending{code: exit.Code}
	}
	if name, ok := signalNames[exit.Signal]; ok {
		return ending{signal: name}
	}

	// A shell's code for a signal the RFC has no name for.
	return ending{code: 128 + exit.Signal}
}

func exitCode(code int) uint32 {
	if code < 0 || code > math.MaxUint8 {
		return refusedExitCode
	}

	return uint32(code)
}

func dimension(n uint32) uint16 {
	if n > math.MaxUint16 {
		return math.MaxUint16
	}

	return uint16(n)
}

// gone says err is the client or the connection having left first, which ends a session and is no fault.
func gone(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}
