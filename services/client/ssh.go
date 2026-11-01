package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/api"
)

// SSHPin is where ssh keeps the host key of one daemon, and the one name it files that key under for every sandbox.
type SSHPin struct {
	KnownHosts string
	Alias      string
}

// PinFor is the pin of the daemon a client of root or remote reaches: local for the default socket, else a short hash of the root or of the remote.
func PinFor(getenv func(string) string, root, remote string) (SSHPin, error) {
	config, err := ConfigPath(getenv)
	if err != nil {
		return SSHPin{}, err
	}

	id, err := daemonID(root, remote)
	if err != nil {
		return SSHPin{}, err
	}

	return SSHPin{KnownHosts: filepath.Join(filepath.Dir(config), "ssh", "known_hosts-"+id), Alias: "shard-" + id}, nil
}

// daemonID names a remote by what the client dials, as sameRemote does, so a path or the case of a host is the same daemon.
func daemonID(root, remote string) (string, error) {
	if remote != "" {
		parsed, err := parseRemote(remote)
		if err != nil {
			return "", err
		}

		return "remote-" + shortHash(parsed.Scheme+"://"+strings.ToLower(remoteAddress(parsed))), nil
	}
	if root == DefaultRoot {
		return "local", nil
	}

	return "local-" + shortHash(filepath.Clean(root)), nil
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:6])
}

// Pin makes key the one host key of the daemon, so ssh checks the daemon's key against what the API, already trusted, said it is.
func (p SSHPin) Pin(key ssh.PublicKey) error {
	dir := filepath.Dir(p.KnownHosts)
	if err := store.MkdirAllDurable(dir, 0o700); err != nil {
		return fmt.Errorf("create %s for the ssh host keys: %w", dir, err)
	}

	line := knownhosts.Line([]string{p.Alias}, key) + "\n"
	if err := store.WriteFileIfChanged(p.KnownHosts, []byte(line), 0o600); err != nil {
		return fmt.Errorf("pin the daemon's ssh host key in %s: %w", p.KnownHosts, err)
	}

	return nil
}

// SSH opens one ssh connection into the running sandbox ref, and answers it with the host key the daemon sent on the 101.
func (c *Client) SSH(ctx context.Context, ref string) (net.Conn, ssh.PublicKey, error) {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/ssh"

	conn, header, err := c.upgrade(ctx, path, "an ssh connection to sandbox "+ref)
	if err != nil {
		return nil, nil, missing(ref, err)
	}

	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(header.Get(api.SSHHostKeyHeader)))
	if err != nil {
		err = fmt.Errorf("the daemon at %s sent no ssh host key shard can read in %s: %w", c.target, api.SSHHostKeyHeader, err)

		return nil, nil, errors.Join(err, conn.CloseNow())
	}

	return websocket.NetConn(ctx, conn, websocket.MessageBinary), key, nil
}
