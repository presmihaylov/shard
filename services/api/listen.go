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
	"strconv"
	"time"
)

// SocketFile is the unix socket under the root the daemon answers on.
const SocketFile = "shard.sock"

// Group is the host group that reaches the socket without root, when the host has it.
const Group = "shard"

const (
	groupPerm    = 0o660
	rootOnlyPerm = 0o600
	// shutdownWait bounds a slow header on the way in and the drain of in-flight answers on a stop.
	shutdownWait = 5 * time.Second
)

// Listen opens the socket at path and reports the mode it chose, so the journal says who can reach it.
func Listen(path string) (net.Listener, string, error) {
	// The lock, not the file, says whether a daemon runs, so a socket an earlier one left is in the way.
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, "", fmt.Errorf("remove the stale socket %s: %w", path, err)
	}

	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, "", fmt.Errorf("listen on %s: %w", path, err)
	}

	mode, err := secure(path)
	if err != nil {
		return nil, "", errors.Join(err, l.Close())
	}

	return l, mode, nil
}

// secure narrows the socket to the shard group when the host has one, else to root: looked up by name, no config.
func secure(path string) (string, error) {
	group, err := user.LookupGroup(Group)
	var unknown user.UnknownGroupError
	if errors.As(err, &unknown) {
		if err := os.Chmod(path, rootOnlyPerm); err != nil {
			return "", fmt.Errorf("chmod %s: %w", path, err)
		}

		return fmt.Sprintf("mode %04o, root only: the host has no %s group", rootOnlyPerm, Group), nil
	}
	if err != nil {
		return "", fmt.Errorf("look up the %s group: %w", Group, err)
	}

	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return "", fmt.Errorf("the %s group has the gid %q: %w", Group, group.Gid, err)
	}
	if err := os.Chown(path, -1, gid); err != nil {
		return "", fmt.Errorf("chown %s to the %s group: %w", path, Group, err)
	}
	if err := os.Chmod(path, groupPerm); err != nil {
		return "", fmt.Errorf("chmod %s: %w", path, err)
	}

	return fmt.Sprintf("mode %04o, group %s", groupPerm, Group), nil
}

// Serve answers on l until ctx ends, then closes the listener and drains what is in flight.
func Serve(ctx context.Context, l net.Listener, h http.Handler) error {
	server := &http.Server{Handler: h, ReadHeaderTimeout: shutdownWait}

	done := make(chan error, 1)
	go func() { done <- server.Serve(l) }()

	select {
	case err := <-done:
		return fmt.Errorf("serve the socket: %w", err)
	case <-ctx.Done():
	}

	drain, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()

	if err := server.Shutdown(drain); err != nil {
		return fmt.Errorf("stop the socket: %w", err)
	}

	return nil
}
