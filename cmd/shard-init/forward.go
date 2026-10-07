package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/pkg/splice"
	"github.com/presmihaylov/shard/services/supervisor"
)

// forwardDialTimeout bounds the dial to the guest's loopback; a port nothing listens on refuses at once, so this only catches a stuck stack.
const forwardDialTimeout = 10 * time.Second

// acceptForward gives every forwarded connection its own stream, as an exec has.
func acceptForward(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: accept a forward connection:", err)

			return
		}
		go serveForward(conn)
	}
}

// serveForward dials the port the header names on the guest's own loopback, answers whether that took, and splices the two.
func serveForward(conn net.Conn) {
	var header supervisor.ForwardHeader
	if err := supervisor.ReadHeader(conn, &header); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: read a forward header:", err)
		closeForward(conn)

		return
	}

	local, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(header.Port))), forwardDialTimeout)
	if err != nil {
		if err := supervisor.WriteMessage(conn, supervisor.ForwardReply{Error: err.Error(), Refused: errors.Is(err, syscall.ECONNREFUSED)}); err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: refuse a forward:", err)
		}
		closeForward(conn)

		return
	}
	if err := supervisor.WriteMessage(conn, supervisor.ForwardReply{}); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: answer a forward:", err)
		closeForward(conn)
		closeForward(local)

		return
	}

	if err := splice.Conns(local, supervisor.NewForwardGuest(conn)); err != nil {
		fmt.Fprintf(os.Stderr, "shard-init: forward to port %d: %v\n", header.Port, err)
	}
}

func closeForward(conn net.Conn) {
	if err := conn.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: close a forward connection:", err)
	}
}
