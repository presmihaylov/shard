package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
)

// sshEnv keeps the shell that runs the test out of it: no remote, and a config directory of the test's own.
func sshEnv(t *testing.T) string {
	t.Helper()
	noRemoteEnv(t)
	config := t.TempDir()
	t.Setenv(client.ConfigHomeEnv, config)

	return config
}

// self is this binary as a ProxyCommand names it.
func self(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	return strings.ReplaceAll(shellQuote(path), "%", "%%")
}

func TestSSHConfigSendsEveryShardHostThroughTheProxy(t *testing.T) {
	config := sshEnv(t)
	var out bytes.Buffer

	if err := (App{Version: "test", Out: &out}).Run(t.Context(), []string{"ssh-config"}); err != nil {
		t.Fatalf("ssh-config: %v", err)
	}

	want := strings.Join([]string{
		"Host *.shard",
		"  ProxyCommand " + self(t) + " --remote '' --root /var/lib/shard ssh-proxy -- %n",
		"  UserKnownHostsFile " + filepath.Join(config, "shard", "ssh", "known_hosts-local"),
		"  HostKeyAlias shard-local",
		"  StrictHostKeyChecking yes",
		"  User root",
	}, "\n") + "\n"
	if out.String() != want {
		t.Errorf("ssh-config printed\n%s\nwant\n%s", out.String(), want)
	}
}

// The remote reaches the ProxyCommand whether a flag or the environment named it, with no password, and its pin is its own.
func TestSSHConfigCarriesTheRemoteAndNoSecret(t *testing.T) {
	cases := map[string]func(t *testing.T) []string{
		"--remote": func(*testing.T) []string {
			return []string{"--remote", "https://ops:hunter2@shard.example.com", "ssh-config"}
		},
		client.RemoteEnv: func(t *testing.T) []string {
			t.Setenv(client.RemoteEnv, "https://ops:hunter2@shard.example.com")

			return []string{"ssh-config"}
		},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			sshEnv(t)
			var out bytes.Buffer

			if err := (App{Version: "test", Out: &out}).Run(t.Context(), args(t)); err != nil {
				t.Fatalf("ssh-config: %v", err)
			}

			printed := out.String()
			if want := " --remote https://shard.example.com ssh-proxy -- %n\n"; !strings.Contains(printed, want) {
				t.Errorf("ssh-config printed\n%s\nwant a ProxyCommand ending in %q", printed, want)
			}
			if strings.Contains(printed, "hunter2") || strings.Contains(printed, "--root") {
				t.Errorf("ssh-config printed\n%s\nwith the password or a --root", printed)
			}
			if !strings.Contains(printed, "HostKeyAlias shard-remote-") || !strings.Contains(printed, "known_hosts-remote-") {
				t.Errorf("ssh-config printed\n%s\nwant the pin of the remote", printed)
			}
		})
	}
}

// A root with a blank or a % stays one word to the shell and to ssh, and a line break never reaches the config.
func TestSSHConfigQuotesWhatSSHAndTheShellWouldSplit(t *testing.T) {
	sshEnv(t)
	var out bytes.Buffer

	if err := (App{Version: "test", Out: &out}).Run(t.Context(), []string{"--root", "/srv/100% it's", "ssh-config"}); err != nil {
		t.Fatalf("ssh-config: %v", err)
	}
	if want := ` --root '/srv/100%% it'\''s' ssh-proxy -- %n`; !strings.Contains(out.String(), want) {
		t.Errorf("ssh-config printed\n%s\nwant %s", out.String(), want)
	}

	err := (App{Version: "test", Out: io.Discard}).Run(t.Context(), []string{"--root", "/srv/a\nProxyCommand evil", "ssh-config"})
	if err == nil || !strings.Contains(err.Error(), "line break") {
		t.Errorf("a root with a line break returned %v, want a refusal", err)
	}

	quoted := map[string]string{
		"/home/a b/known_hosts":  `"/home/a b/known_hosts"`,
		`/x/"q"\y`:               `"/x/\"q\"\\y"`,
		"/home/me/known_hosts-1": "/home/me/known_hosts-1",
	}
	for value, want := range quoted {
		if got := configQuote(value); got != want {
			t.Errorf("configQuote(%q) = %s, want %s", value, got, want)
		}
	}
}

// fakeSSH puts an ssh on PATH, so LookPath finds one, and answers its path.
func fakeSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // G306: LookPath wants it executable
		t.Fatalf("write the fake ssh: %v", err)
	}
	t.Setenv("PATH", dir)

	return path
}

func TestSSHExecsTheClientWithTheConfigsOptions(t *testing.T) {
	sshEnv(t)
	path := fakeSSH(t)

	options, err := (App{Root: DefaultRoot}).sshOptions()
	if err != nil {
		t.Fatalf("sshOptions: %v", err)
	}
	var ours []string
	for _, o := range options {
		ours = append(ours, "-o", o.key+"="+o.value)
	}

	cases := map[string]struct {
		args []string
		tail []string
	}{
		"a shell as root":          {[]string{"ssh", "web1"}, []string{"-l", "root", "--", "web1.shard"}},
		"the host as ssh names it": {[]string{"ssh", "web1.shard"}, []string{"-l", "root", "--", "web1.shard"}},
		"a command, a forward and a user": {
			[]string{"ssh", "--user", "app", "web1", "-L", "9222:localhost:9222", "-vp22", "--", "pg_dump", "-v", "app"},
			[]string{"-l", "app", "-L", "9222:localhost:9222", "-vp22", "--", "web1.shard", "pg_dump", "-v", "app"},
		},
		"a name that starts with -": {[]string{"ssh", "--", "-web"}, []string{"-l", "root", "--", "-web.shard"}},
	}
	for name, c := range cases {
		var ran string
		var argv []string
		app := App{Version: "test", Out: io.Discard, replace: func(p string, a, _ []string) error {
			ran, argv = p, a
			return nil
		}}

		if err := app.Run(t.Context(), c.args); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := append(append([]string{"ssh"}, ours...), c.tail...)
		if ran != path || !slices.Equal(argv, want) {
			t.Errorf("%s ran %s %q, want %s %q", name, ran, argv, path, want)
		}
	}
}

// A word ssh would take for the host would reach another sandbox, so shard refuses it before ssh runs.
func TestSSHRefusesWhatSSHWouldReadAsAnotherHost(t *testing.T) {
	sshEnv(t)
	fakeSSH(t)

	cases := map[string]struct {
		args []string
		want string
	}{
		"no sandbox":              {[]string{"ssh"}, "ssh takes a sandbox"},
		"a command with no --":    {[]string{"ssh", "web1", "ls"}, `"ls" is no ssh option; put a command after --`},
		"a value-less -L":         {[]string{"ssh", "web1", "-L"}, "ssh option -L needs a value"},
		"ssh's -l":                {[]string{"ssh", "web1", "-vl", "bob"}, "pick the user with --user"},
		"a shard flag after it":   {[]string{"ssh", "web1", "--user", "bob"}, "--user goes before the sandbox"},
		"an empty user":           {[]string{"ssh", "--user", "", "web1"}, "--user needs a user name"},
		"an ssh option before it": {[]string{"ssh", "-L", "1:localhost:1", "web1"}, "unknown flag -L"},
	}
	for name, c := range cases {
		app := App{Version: "test", Out: io.Discard, replace: func(string, []string, []string) error {
			t.Errorf("%s ran ssh", name)
			return nil
		}}

		err := app.Run(t.Context(), c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s returned %v, want %q", name, err, c.want)
		}
	}
}

func TestSSHWithNoClientOnPathSaysSo(t *testing.T) {
	sshEnv(t)
	t.Setenv("PATH", t.TempDir())

	err := (App{Version: "test", Out: io.Discard}).Run(t.Context(), []string{"ssh", "web1"})
	if err == nil || !strings.Contains(err.Error(), "found no ssh on PATH") {
		t.Errorf("ssh with no client returned %v, want it to say so", err)
	}
}

// pinCheck is ssh's end of the proxy: at the first byte it reads, the pin must already hold the daemon's key.
type pinCheck struct {
	pin  client.SSHPin
	want string

	mu     sync.Mutex
	got    bytes.Buffer
	pinned bool
}

func (p *pinCheck) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.got.Len() == 0 {
		data, err := os.ReadFile(p.pin.KnownHosts)
		p.pinned = err == nil && string(data) == p.want
	}

	return p.got.Write(b)
}

// sshDaemon serves the ssh route on root's socket: it sends a banner, waits for the client's line, and hangs up.
func sshDaemon(t *testing.T, root string, key ssh.PublicKey) {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(root, api.SocketFile))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/sandboxes/web1/ssh" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"sandbox ghost not found"}}`)

			return
		}
		w.Header().Set(api.SSHHostKeyHeader, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))))
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)

			return
		}
		stream := websocket.NetConn(r.Context(), conn, websocket.MessageBinary)
		if _, err := io.WriteString(stream, "SSH-2.0-sandbox\r\n"); err != nil {
			t.Errorf("send the banner: %v", err)
		}
		line := make([]byte, len("SSH-2.0-client\r\n"))
		if _, err := io.ReadFull(stream, line); err != nil || string(line) != "SSH-2.0-client\r\n" {
			t.Errorf("the daemon read %q, %v", line, err)
		}
		if err := stream.Close(); err != nil {
			t.Errorf("hang up: %v", err)
		}
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
}

func TestSSHProxyPinsTheKeyBeforeItRelaysAByte(t *testing.T) {
	sshEnv(t)
	root := shortRoot(t)
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatalf("wrap the key: %v", err)
	}
	sshDaemon(t, root, key)

	pin, err := client.PinFor(os.Getenv, root, "")
	if err != nil {
		t.Fatalf("PinFor: %v", err)
	}
	in, typed, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	// The relay's read of stdin outlives Run, as it would in the process ssh starts, until the test hangs up.
	t.Cleanup(func() {
		if err := errors.Join(typed.Close(), in.Close()); err != nil {
			t.Errorf("close stdin: %v", err)
		}
	})
	if _, err := io.WriteString(typed, "SSH-2.0-client\r\n"); err != nil {
		t.Fatalf("type: %v", err)
	}

	out := &pinCheck{pin: pin, want: pin.Alias + " " + string(ssh.MarshalAuthorizedKey(key))}
	app := App{Version: "test", Root: root, Out: out, in: in}
	if err := app.Run(t.Context(), []string{"ssh-proxy", "web1.shard"}); err != nil {
		t.Fatalf("ssh-proxy: %v", err)
	}

	out.mu.Lock()
	defer out.mu.Unlock()
	if out.got.String() != "SSH-2.0-sandbox\r\n" || !out.pinned {
		t.Errorf("ssh read %q, pinned before it %v; want the banner, after the pin", out.got.String(), out.pinned)
	}
}

// A refused route pins nothing, so a key never lands for a connection that did not happen.
func TestSSHProxyToNoSuchSandboxPinsNothing(t *testing.T) {
	sshEnv(t)
	root := shortRoot(t)
	sshDaemon(t, root, nil)

	err := (App{Version: "test", Root: root, Out: io.Discard}).Run(t.Context(), []string{"ssh-proxy", "ghost.shard"})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("ssh-proxy to ghost returned %v, want it not found", err)
	}

	pin, err := client.PinFor(os.Getenv, root, "")
	if err != nil {
		t.Fatalf("PinFor: %v", err)
	}
	if _, err := os.Stat(pin.KnownHosts); !os.IsNotExist(err) {
		t.Errorf("a refused proxy left %s: %v", pin.KnownHosts, err)
	}
}
