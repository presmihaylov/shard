//go:build linux

package netns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
)

// DialIn opens one TCP connection to port on the loopback of the network namespace at path; the socket keeps that namespace once the thread is home.
func DialIn(ctx context.Context, path string, port uint16) (net.Conn, error) {
	type dialed struct {
		conn net.Conn
		err  error
	}
	done := make(chan dialed, 1)
	go func() {
		runtime.LockOSThread()
		var conn net.Conn
		home, err := inNamespaceAt(path, func() error {
			var d net.Dialer
			opened, err := d.DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
			conn = opened

			return err
		})
		// A thread left in the namespace stays locked, so the runtime ends it with this goroutine.
		if home {
			runtime.UnlockOSThread()
		}
		done <- dialed{conn, err}
	}()

	res := <-done
	if res.err == nil {
		return res.conn, nil
	}
	err := fmt.Errorf("dial port %d in %s: %w", port, path, res.err)
	if res.conn != nil {
		return nil, errors.Join(err, res.conn.Close())
	}

	return nil, err
}
