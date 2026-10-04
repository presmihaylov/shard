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
	// traverseMode lets the group reach the socket by its name, and list or read nothing else under the root.
	traverseMode = fs.FileMode(0o710)
	// readHeaderTimeout bounds a client that connects and sends nothing, so it cannot hold a slot forever.
	readHeaderTimeout = 10 * time.Second
	// readTimeout bounds a slow body, and the gap between the reads of a streamed put; net/http clears it once the body is in.
	readTimeout   = 30 * time.Second
	shutdownGrace = 5 * time.Second
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
	g, err := user.LookupGroup(group)

	var unknown user.UnknownGroupError
	if errors.As(err, &unknown) {
		if err := os.Chmod(path, rootMode); err != nil {
			return 0, "", fmt.Errorf("set the mode of %s: %w", path, err)
		}

		return rootMode, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("look up the group %s: %w", group, err)
	}

	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, "", fmt.Errorf("parse the gid %q of the group %s: %w", g.Gid, group, err)
	}
	if err := os.Chown(path, -1, gid); err != nil {
		return 0, "", fmt.Errorf("give %s to the group %s: %w", path, group, err)
	}
	if err := os.Chmod(path, groupMode); err != nil {
		return 0, "", fmt.Errorf("set the mode of %s: %w", path, err)
	}

	// The unit's UMask=0077 leaves the root 0700, which keeps the group off the socket; it needs to traverse and nothing more (SHARD-468).
	root := filepath.Dir(path)
	if err := os.Chown(root, -1, gid); err != nil {
		return 0, "", fmt.Errorf("give the root %s to the group %s: %w", root, group, err)
	}
	if err := os.Chmod(root, traverseMode); err != nil {
		return 0, "", fmt.Errorf("set the mode of the root %s: %w", root, err)
	}

	return groupMode, group, nil
}

// Serve returns nil once ctx ends; any other end is the listener dying, an error so the daemon restarts it.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	server := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout}

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
