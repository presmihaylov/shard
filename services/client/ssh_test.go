package client_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
)

func hostKey(t *testing.T) ssh.PublicKey {
	t.Helper()

	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatalf("wrap the key: %v", err)
	}

	return key
}

// echoSSH is a daemon's ssh route that sends header as the host key and echoes the stream.
func echoSSH(t *testing.T, header string) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/sandboxes/web1/ssh" {
			t.Errorf("the client opened %s", r.URL.Path)
		}
		w.Header().Set(api.SSHHostKeyHeader, header)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)

			return
		}
		defer conn.CloseNow()
		// The client ends the stream, so how the echo ends is not under test.
		stream := websocket.NetConn(r.Context(), conn, websocket.MessageBinary)
		_, _ = io.Copy(stream, stream)
	}
}

func TestSSHAnswersTheHostKeyAndCarriesTheBytes(t *testing.T) {
	key := hostKey(t)
	c := serve(t, shortRoot(t), echoSSH(t, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))))

	conn, got, err := c.SSH(t.Context(), "web1")
	if err != nil {
		t.Fatalf("SSH: %v", err)
	}
	if !bytes.Equal(got.Marshal(), key.Marshal()) {
		t.Errorf("SSH answered the key %s, want the one the header carries", ssh.FingerprintSHA256(got))
	}

	if _, err := io.WriteString(conn, "SSH-2.0-test\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	echoed := make([]byte, len("SSH-2.0-test\r\n"))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(echoed) != "SSH-2.0-test\r\n" {
		t.Errorf("the stream carried %q", echoed)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// A 101 with no key shard can read would leave ssh nothing to check, so the connection never reaches it.
func TestSSHRefusesAnUpgradeWithNoHostKey(t *testing.T) {
	for _, header := range []string{"", "ssh-ed25519 not-base64"} {
		c := serve(t, shortRoot(t), echoSSH(t, header))

		conn, _, err := c.SSH(t.Context(), "web1")
		if err == nil || !strings.Contains(err.Error(), api.SSHHostKeyHeader) {
			t.Errorf("a host key of %q returned %v, want a refusal that names the header", header, err)
		}
		if conn != nil {
			t.Errorf("a host key of %q answered a connection", header)
		}
	}
}

func TestSSHToNoSuchSandboxIsNotFound(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"sandbox ghost not found"}}`))

	_, _, err := c.SSH(t.Context(), "ghost")
	var missing *client.NotFoundError
	if !errors.As(err, &missing) || missing.Ref != "ghost" {
		t.Errorf("SSH into a missing sandbox returned %v, want a NotFoundError for ghost", err)
	}
}

// One pin per daemon: the default socket is local, another root or a remote a short hash of what the client dials.
func TestPinForNamesOneFilePerDaemon(t *testing.T) {
	config := t.TempDir()
	env := func(name string) string {
		if name == client.ConfigHomeEnv {
			return config
		}

		return ""
	}
	pin := func(root, remote string) client.SSHPin {
		t.Helper()
		p, err := client.PinFor(env, root, remote)
		if err != nil {
			t.Fatalf("PinFor(%q, %q): %v", root, remote, err)
		}

		return p
	}

	local := pin(client.DefaultRoot, "")
	if want := filepath.Join(config, "shard", "ssh", "known_hosts-local"); local.KnownHosts != want || local.Alias != "shard-local" {
		t.Errorf("the default socket pins %+v, want %s under shard-local", local, want)
	}

	other := pin("/srv/shard", "")
	if !strings.HasPrefix(other.Alias, "shard-local-") || other.KnownHosts != filepath.Join(config, "shard", "ssh", "known_hosts-"+strings.TrimPrefix(other.Alias, "shard-")) {
		t.Errorf("another root pins %+v, want shard-local-<hash> in its own file", other)
	}

	remote := pin("", "https://shard.example.com")
	for _, same := range []string{"https://Shard.Example.com/", "https://shard.example.com:443/api", "https://ops@shard.example.com"} {
		if got := pin(client.DefaultRoot, same); got != remote {
			t.Errorf("%s pins %+v, want the same daemon as https://shard.example.com, %+v", same, got, remote)
		}
	}
	for _, differs := range []string{"http://shard.example.com", "https://shard.example.com:8443", "https://other.example.com"} {
		if got := pin("", differs); got == remote || got == local {
			t.Errorf("%s pins %+v, which another daemon already has", differs, got)
		}
	}

	if _, err := client.PinFor(env, "", "ftp://shard.example.com"); err == nil {
		t.Error("PinFor took an ftp remote")
	}
}

// The pin is the one line ssh checks the daemon's key against, so a new key replaces the old one rather than joining it.
func TestPinWritesTheOneKeyOfTheDaemon(t *testing.T) {
	pin := client.SSHPin{KnownHosts: filepath.Join(t.TempDir(), "ssh", "known_hosts-local"), Alias: "shard-local"}

	for _, key := range []ssh.PublicKey{hostKey(t), hostKey(t)} {
		if err := pin.Pin(key); err != nil {
			t.Fatalf("Pin: %v", err)
		}

		data, err := os.ReadFile(pin.KnownHosts)
		if err != nil {
			t.Fatalf("read the pin: %v", err)
		}
		if want := "shard-local " + string(ssh.MarshalAuthorizedKey(key)); string(data) != want {
			t.Errorf("the pin holds %q, want %q", data, want)
		}
	}

	for path, want := range map[string]os.FileMode{pin.KnownHosts: 0o600, filepath.Dir(pin.KnownHosts): 0o700} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %04o, want %04o", path, got, want)
		}
	}
}
