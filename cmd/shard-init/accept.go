package main

import (
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// The first retry is quick for a descriptor freed at once, and the cap keeps a lasting failure to one log line a second.
const (
	acceptFloor   = 5 * time.Millisecond
	acceptCeiling = time.Second
)

// retrying keeps a listener serving through a failed Accept: one EMFILE must not end it for the life of the sandbox (SHARD-354).
type retrying struct {
	net.Listener
	closed atomic.Bool
}

func (l *retrying) Accept() (net.Conn, error) {
	wait := acceptFloor
	for {
		conn, err := l.Listener.Accept()
		if err == nil || l.closed.Load() {
			return conn, err
		}
		fmt.Fprintf(os.Stderr, "shard-init: accept on %s, retry in %s: %v\n", l.Addr(), wait, err)
		time.Sleep(wait)
		wait = min(2*wait, acceptCeiling)
	}
}

// Close is the one way out of Accept, since a closed vsock socket fails with a plain file error, not net.ErrClosed.
func (l *retrying) Close() error {
	l.closed.Store(true)

	return l.Listener.Close()
}
