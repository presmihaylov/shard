package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"
)

// SocketFile is the unix socket under the root the daemon answers on. There is no TCP listener.
const SocketFile = "shard.sock"

// Group is the host group that may reach the socket when it exists; otherwise root alone can.
const Group = "shard"

const (
	groupMode = fs.FileMode(0o660)
	rootMode  = fs.FileMode(0o600)
	// readHeaderTimeout bounds a client that connects and sends nothing, so it cannot hold a slot forever.
	readHeaderTimeout = 10 * time.Second
	shutdownGrace     = 5 * time.Second
)

// Listen binds the socket under root and reports the mode it set and the group it gave it, empty without one.
func Listen(root string) (net.Listener, fs.FileMode, string, error) {
	return listen(root, Group)
}

func listen(root, group string) (net.Listener, fs.FileMode, string, error) {
	path := filepath.Join(root, SocketFile)

	if err := removeStale(path); err != nil {
		return nil, 0, "", err
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, 0, "", fmt.Errorf("listen on %s: %w", path, err)
	}

	mode, owner, err := restrict(path, group)
	if err != nil {
		return nil, 0, "", errors.Join(err, listener.Close())
	}

	return listener, mode, owner, nil
}

// removeStale unlinks the socket of a dead daemon: the singleton lock is held, so no live one owns it.
func removeStale(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if info.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("%s is not a socket, refusing to remove it", path)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove the stale socket %s: %w", path, err)
	}

	return nil
}

// restrict gives the socket to group at 0660 when the host has it, else leaves it to root at 0600.
func restrict(path, group string) (fs.FileMode, string, error) {
	mode, owner := rootMode, ""

	g, err := user.LookupGroup(group)

	var unknown user.UnknownGroupError
	if err != nil && !errors.As(err, &unknown) {
		return 0, "", fmt.Errorf("look up the group %s: %w", group, err)
	}
	if err == nil {
		gid, err := strconv.Atoi(g.Gid)
		if err != nil {
			return 0, "", fmt.Errorf("parse the gid %q of the group %s: %w", g.Gid, group, err)
		}
		if err := os.Chown(path, -1, gid); err != nil {
			return 0, "", fmt.Errorf("give %s to the group %s: %w", path, group, err)
		}
		mode, owner = groupMode, group
	}

	if err := os.Chmod(path, mode); err != nil {
		return 0, "", fmt.Errorf("set the mode of %s: %w", path, err)
	}

	return mode, owner, nil
}

// Serve returns nil once ctx ends; any other end is the listener dying, an error so the daemon restarts it.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	server := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}

	served := make(chan struct{})
	shutdown := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			// ctx is already done here, so the grace period must not inherit its cancellation.
			graceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
			defer cancel()
			shutdown <- server.Shutdown(graceCtx)
		case <-served:
			shutdown <- nil
		}
	}()

	err := server.Serve(listener)
	close(served)

	if ctx.Err() == nil {
		return fmt.Errorf("serve the api on %s: %w", listener.Addr(), err)
	}

	if err := <-shutdown; err != nil {
		return fmt.Errorf("shut the api down: %w", err)
	}

	return nil
}
