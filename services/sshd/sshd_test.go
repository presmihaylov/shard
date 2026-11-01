package sshd

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/supervisor"
)

// write is the command writing to its stdout; a failure is the test's, since the client waits for the bytes.
func write(t *testing.T, w io.Writer, text string) {
	t.Helper()
	if _, err := io.WriteString(w, text); err != nil {
		t.Errorf("write %q: %v", text, err)
	}
}

func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read a line: %v", err)
	}

	return strings.TrimRight(line, "\r\n")
}

func TestAShellOnATerminalRunsTheShellScriptAsTheSSHUserAndFollowsItsWindow(t *testing.T) {
	before := runtime.NumGoroutine()
	fake := &fakeSandboxes{command: func(e *fakeExec, streams sandbox.Streams) models.ExitStatus {
		write(t, streams.Stdout, "ready\r\n")
		size := <-e.resizes
		write(t, streams.Stdout, fmt.Sprintf("%dx%d\r\n", size.Rows, size.Cols))

		return models.ExitStatus{}
	}}
	s := serve(t, fake, "app")

	session, err := s.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RequestPty("xterm-256color", 40, 120, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(out)
	if got := readLine(t, lines); got != "ready" {
		t.Fatalf("first line = %q, want ready", got)
	}
	if err := session.WindowChange(50, 160); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, lines); got != "50x160" {
		t.Errorf("the command saw the window %q, want 50x160", got)
	}
	if err := session.Wait(); err != nil {
		t.Errorf("Wait = %v, want a clean exit", err)
	}

	e := fake.only(t)
	want := []string{"sh", "-c", sandbox.ShellScript}
	if !slices.Equal(e.req.Command, want) {
		t.Errorf("command = %q, want %q", e.req.Command, want)
	}
	if e.req.User != "app" || !e.req.TTY || !e.req.Stdin || !e.req.Attach {
		t.Errorf("request = %+v, want user app, a tty, stdin and an attach", e.req)
	}
	if e.req.Size != (sandbox.TerminalSize{Rows: 40, Cols: 120}) {
		t.Errorf("size = %+v, want 40x120", e.req.Size)
	}
	if !slices.Contains(e.req.Env, "TERM=xterm-256color") {
		t.Errorf("env = %q, want the client's TERM", e.req.Env)
	}

	if err := session.Close(); err != nil && !gone(err) {
		t.Fatal(err)
	}
	if err := s.client.Close(); err != nil {
		t.Fatal(err)
	}
	s.wait(t)
	if !e.deleted {
		t.Error("the exec outlived its session")
	}
	settles(t, before)
}

func TestAnExecRunsUnderShWithTheClientsEnvAndEndsWithItsExitStatus(t *testing.T) {
	fake := &fakeSandboxes{command: func(_ *fakeExec, streams sandbox.Streams) models.ExitStatus {
		write(t, streams.Stdout, "out")
		write(t, streams.Stderr, "err")

		return models.ExitStatus{Code: 3}
	}}
	s := serve(t, fake, "root")

	session, err := s.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Setenv("LANG", "C.UTF-8"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	session.Stdout, session.Stderr = &stdout, &stderr
	err = session.Run("pg_dump app; exit 3")

	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 {
		t.Fatalf("Run = %v, want exit status 3", err)
	}
	if stdout.String() != "out" || stderr.String() != "err" {
		t.Errorf("stdout %q stderr %q, want out and err apart without a terminal", stdout.String(), stderr.String())
	}
	e := fake.only(t)
	if want := []string{"sh", "-c", "pg_dump app; exit 3"}; !slices.Equal(e.req.Command, want) {
		t.Errorf("command = %q, want %q", e.req.Command, want)
	}
	if e.req.TTY || e.req.User != "root" || !slices.Equal(e.req.Env, []string{"LANG=C.UTF-8"}) {
		t.Errorf("request = %+v, want root, no tty and the client's LANG", e.req)
	}
}

func TestASignalReachesTheCommandAndItsDeathClosesTheChannelWithExitSignal(t *testing.T) {
	fake := &fakeSandboxes{command: func(e *fakeExec, streams sandbox.Streams) models.ExitStatus {
		write(t, streams.Stdout, "ready\n")
		if sig := <-e.signals; sig != "TERM" {
			t.Errorf("signal = %q, want TERM", sig)
		}

		return models.ExitStatus{Signal: int(syscall.SIGTERM)}
	}}
	s := serve(t, fake, "root")

	session, err := s.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start("sleep 600"); err != nil {
		t.Fatal(err)
	}
	readLine(t, bufio.NewReader(out))
	if err := session.Signal(ssh.SIGTERM); err != nil {
		t.Fatal(err)
	}

	var exit *ssh.ExitError
	if err := session.Wait(); !errors.As(err, &exit) || exit.Signal() != "TERM" {
		t.Fatalf("Wait = %v, want an exit-signal TERM", err)
	}
}

func TestTheClientsEOFClosesTheCommandsStdin(t *testing.T) {
	fake := &fakeSandboxes{command: func(_ *fakeExec, streams sandbox.Streams) models.ExitStatus {
		if _, err := io.Copy(streams.Stdout, streams.Stdin); err != nil {
			t.Errorf("copy stdin: %v", err)
		}

		return models.ExitStatus{}
	}}
	s := serve(t, fake, "root")

	session, err := s.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	session.Stdin = strings.NewReader("a whole file\n")
	got, err := session.Output("cat")
	if err != nil {
		t.Fatalf("Output = %v; the command never saw its stdin end", err)
	}
	if string(got) != "a whole file\n" {
		t.Errorf("output = %q", got)
	}
}

func TestAnUnknownUserIsRefusedOnStderrWithANonZeroExit(t *testing.T) {
	fake := &fakeSandboxes{createErr: &bundle.UnknownUserError{Err: errors.New(`resolve the user "ghost": no such user`)}}
	s := serve(t, fake, "ghost")

	session, err := s.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	session.Stderr = &stderr
	err = session.Run("id")

	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != refusedExitCode {
		t.Fatalf("Run = %v, want exit status %d", err, refusedExitCode)
	}
	if !strings.Contains(stderr.String(), `resolve the user "ghost"`) {
		t.Errorf("stderr = %q, want the refusal", stderr.String())
	}
}

func TestAnAttachRefusedWithInputQueuedEndsTheSession(t *testing.T) {
	fake := &fakeSandboxes{attachErr: models.NotFound(nil, "sandbox web1 not found")}
	s := serve(t, fake, "root")

	session, err := s.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Sent before the exec, so the input is queued when the attach refuses.
	write(t, stdin, "queued\n")
	err = session.Run("cat")
	if exit, ok := errors.AsType[*ssh.ExitError](err); !ok || exit.ExitStatus() != refusedExitCode {
		t.Fatalf("Run = %v, want exit %d", err, refusedExitCode)
	}

	if err := s.client.Close(); err != nil {
		t.Fatal(err)
	}
	s.wait(t)
}

func TestACommandThatDidNotStartExitsWithItsOwnCode(t *testing.T) {
	fake := &fakeSandboxes{createErr: &models.CommandNotStartedError{Sandbox: "web1", Command: "sh", Reason: "no such file", Code: 127}}
	s := serve(t, fake, "root")

	session, err := s.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var exit *ssh.ExitError
	if err := session.Run("true"); !errors.As(err, &exit) || exit.ExitStatus() != 127 {
		t.Fatalf("Run = %v, want exit status 127", err)
	}
}

func TestTheSFTPSubsystemRunsShardInitAsTheSSHUserOverTheChannel(t *testing.T) {
	home := t.TempDir()
	fake := &fakeSandboxes{command: func(_ *fakeExec, streams sandbox.Streams) models.ExitStatus {
		server, err := sftp.NewServer(pipe{streams.Stdin, streams.Stdout}, sftp.WithServerWorkingDirectory(home))
		if err != nil {
			t.Errorf("start sftp: %v", err)

			return models.ExitStatus{Code: 1}
		}
		if err := server.Serve(); err != nil && !gone(err) {
			t.Errorf("serve sftp: %v", err)
		}

		return models.ExitStatus{}
	}}
	s := serve(t, fake, "app")

	client, err := sftp.NewClient(s.client)
	if err != nil {
		t.Fatal(err)
	}
	f, err := client.Create("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("copied")); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(f.Close(), client.Close()); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(home, "a.txt"))
	if err != nil || string(got) != "copied" {
		t.Fatalf("the file reads %q, %v; want what scp sent", got, err)
	}
	e := fake.only(t)
	if want := []string{supervisor.InitPath, supervisor.SFTPMode}; !slices.Equal(e.req.Command, want) {
		t.Errorf("command = %q, want %q", e.req.Command, want)
	}
	if e.req.User != "app" || e.req.TTY {
		t.Errorf("request = %+v, want user app and no terminal", e.req)
	}
}

// pipe is the attach's stdin and stdout as the one stream an sftp server speaks over.
type pipe struct {
	io.Reader
	io.Writer
}

func (pipe) Close() error { return nil }

func TestAForwardToTheSandboxsLoopbackReachesTheSandboxsPort(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			fake := &fakeSandboxes{dial: func(uint16) (net.Conn, error) {
				guest, conn := net.Pipe()
				go func() {
					if _, err := io.Copy(guest, guest); err != nil && !gone(err) {
						t.Errorf("echo: %v", err)
					}
				}()

				return conn, nil
			}}
			s := serve(t, fake, "root")

			conn, err := s.client.Dial("tcp", net.JoinHostPort(host, "9222"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, 4)
			if _, err := io.ReadFull(conn, got); err != nil || string(got) != "ping" {
				t.Fatalf("read %q, %v; want the echo", got, err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			if got := fake.dials(); !slices.Equal(got, []uint16{9222}) {
				t.Errorf("dialed %v, want 9222", got)
			}
		})
	}
}

func TestAForwardEndsWhenItsClientClosesThoughTheGuestHoldsItsSideOpen(t *testing.T) {
	before := runtime.NumGoroutine()
	guests := make(chan net.Conn, 1)
	fake := &fakeSandboxes{dial: func(uint16) (net.Conn, error) {
		conn, guest := loopbackPair(t)
		guests <- guest

		return conn, nil
	}}
	s := serve(t, fake, "root")

	conn, err := s.client.Dial("tcp", "localhost:9222")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(<-guests)
	if err != nil || string(got) != "ping" {
		t.Fatalf("guest read %q, %v; want ping then the end", got, err)
	}

	if err := s.client.Close(); err != nil {
		t.Fatal(err)
	}
	s.wait(t)
	settles(t, before)
}

func TestAForwardToAnyOtherHostIsRefusedWithoutADial(t *testing.T) {
	for _, host := range []string{"db.internal", "10.0.0.5", "example.com", "0.0.0.0"} {
		t.Run(host, func(t *testing.T) {
			fake := &fakeSandboxes{}
			s := serve(t, fake, "root")

			_, err := s.client.Dial("tcp", net.JoinHostPort(host, "5432"))
			var refused *ssh.OpenChannelError
			if !errors.As(err, &refused) || refused.Reason != ssh.Prohibited || !strings.Contains(refused.Message, "use localhost") {
				t.Fatalf("Dial = %v, want a prohibited open that names localhost", err)
			}
			if got := fake.dials(); len(got) != 0 {
				t.Errorf("dialed %v for a refused host", got)
			}
		})
	}
}

func TestAForwardToAPortNothingListensOnSaysSo(t *testing.T) {
	fake := &fakeSandboxes{dial: func(uint16) (net.Conn, error) {
		return nil, fmt.Errorf("dial the sandbox: %w", syscall.ECONNREFUSED)
	}}
	s := serve(t, fake, "root")

	_, err := s.client.Dial("tcp", "localhost:9")
	var refused *ssh.OpenChannelError
	if !errors.As(err, &refused) || refused.Reason != ssh.ConnectionFailed || !strings.Contains(refused.Message, "nothing listens on 127.0.0.1:9") {
		t.Fatalf("Dial = %v, want the connection failed for nothing listening", err)
	}
}

func TestARemoteForwardAnAgentAX11AndAUnixSocketAreRefused(t *testing.T) {
	s := serve(t, &fakeSandboxes{}, "root")

	if _, err := s.client.Listen("tcp", "127.0.0.1:8080"); err == nil {
		t.Error("a -R forward was granted")
	}
	if _, err := s.client.Dial("unix", "/run/docker.sock"); err == nil {
		t.Error("a forward to a unix socket was granted")
	}
	if ok, _, err := s.client.SendRequest("streamlocal-forward@openssh.com", true, ssh.Marshal(struct{ Path string }{"/tmp/s"})); ok || err != nil {
		t.Errorf("streamlocal-forward = %v, %v; want a refusal", ok, err)
	}

	session, err := s.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []string{"auth-agent-req@openssh.com", "x11-req", "subsystem-of-nothing"} {
		ok, err := session.SendRequest(request, true, nil)
		if ok || err != nil {
			t.Errorf("%s = %v, %v; want a refusal", request, ok, err)
		}
	}
	if err := session.RequestSubsystem("netconf"); err == nil {
		t.Error("a subsystem other than sftp started")
	}
}

func TestAKeepaliveIsAnsweredAndTheConnectionLives(t *testing.T) {
	fake := &fakeSandboxes{command: func(*fakeExec, sandbox.Streams) models.ExitStatus { return models.ExitStatus{} }}
	s := serve(t, fake, "root")

	if _, _, err := s.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	session, err := s.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Run("true"); err != nil {
		t.Errorf("a command after the keepalive: %v", err)
	}
}

func TestAClientThatLeavesMidCommandEndsItsExecAndLeavesNoGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	fake := &fakeSandboxes{command: func(e *fakeExec, streams sandbox.Streams) models.ExitStatus {
		write(t, streams.Stdout, "ready\n")
		for sig := range e.signals {
			if sig == "KILL" {
				return models.ExitStatus{Signal: int(syscall.SIGKILL)}
			}
		}

		return models.ExitStatus{}
	}}
	s := serve(t, fake, "root")

	session, err := s.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start("sleep 600"); err != nil {
		t.Fatal(err)
	}
	readLine(t, bufio.NewReader(out))
	if err := s.client.Close(); err != nil {
		t.Fatal(err)
	}
	s.wait(t)

	if e := fake.only(t); !e.deleted {
		t.Error("the exec outlived the client")
	}
	if n := fake.attached.Load(); n != 0 {
		t.Errorf("%d attaches still open", n)
	}
	settles(t, before)
}

func TestServeEndsEveryChannelWhenItsContextEnds(t *testing.T) {
	before := runtime.NumGoroutine()
	fake := &fakeSandboxes{
		command: func(e *fakeExec, streams sandbox.Streams) models.ExitStatus {
			write(t, streams.Stdout, "ready\n")
			<-e.signals

			return models.ExitStatus{Signal: int(syscall.SIGKILL)}
		},
		dial: func(uint16) (net.Conn, error) {
			conn, guest := net.Pipe()
			go func() {
				if _, err := io.Copy(io.Discard, guest); err != nil && !gone(err) {
					t.Errorf("guest: %v", err)
				}
			}()

			return conn, nil
		},
	}
	s := serve(t, fake, "root")

	session, err := s.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start("sleep 600"); err != nil {
		t.Fatal(err)
	}
	readLine(t, bufio.NewReader(out))
	if _, err := s.client.Dial("tcp", "localhost:80"); err != nil {
		t.Fatal(err)
	}

	s.cancel()
	s.wait(t)
	if err := session.Wait(); err == nil {
		t.Error("the session ended cleanly though the daemon hung up")
	}
	if err := s.client.Close(); err != nil && !gone(err) {
		t.Fatal(err)
	}
	if e := fake.only(t); !e.deleted {
		t.Error("the exec outlived the connection")
	}
	settles(t, before)
}

func TestTheConnectionStopsAtItsSessionBound(t *testing.T) {
	fake := &fakeSandboxes{}
	s := serve(t, fake, "root")

	for range maxSessions {
		if _, err := s.client.NewSession(); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.client.NewSession()
	var refused *ssh.OpenChannelError
	if !errors.As(err, &refused) || refused.Reason != ssh.ResourceShortage {
		t.Fatalf("session %d = %v, want a resource shortage", maxSessions+1, err)
	}
}

func TestTheHostKeyIsMadeOnceAndReadOnEveryStart(t *testing.T) {
	root := t.TempDir()
	first, err := LoadHostKey(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, HostKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the key file is %v, want 0600", info.Mode().Perm())
	}

	again, err := LoadHostKey(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.PublicKey().Marshal(), again.PublicKey().Marshal()) {
		t.Error("a restart made a new host key, which every client would refuse")
	}
	if first.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Errorf("key type = %s, want ed25519", first.PublicKey().Type())
	}

	line := New(first, &fakeSandboxes{}, nil).HostKey()
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(rest) != 0 || strings.HasSuffix(line, "\n") {
		t.Fatalf("HostKey = %q, %v; want one authorized_keys line", line, err)
	}
	if !bytes.Equal(key.Marshal(), first.PublicKey().Marshal()) {
		t.Error("HostKey names another key")
	}
}

func TestAHostKeyFileThatIsNoKeyIsRefusedNotReplaced(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, HostKeyFile)
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadHostKey(root); err == nil || !strings.Contains(err.Error(), "remove it") {
		t.Fatalf("LoadHostKey = %v, want a refusal that says what to do", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "not a key" {
		t.Errorf("the file reads %q, %v; it was replaced", got, err)
	}
}
