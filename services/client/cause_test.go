package client_test

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/services/client"
)

// A network failure reads as a person says it, and never with the socket addresses around it. (SHARD-672, SHARD-673)
func TestDialCause(t *testing.T) {
	cut := &net.OpError{
		Op:     "read",
		Net:    "tcp",
		Source: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 51234},
		Addr:   &net.TCPAddr{IP: net.IPv4(192, 0, 2, 20), Port: 443},
		Err:    os.NewSyscallError("read", syscall.ETIMEDOUT),
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "shard.invalid", IsNotFound: true}}, "no such host"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, "connection refused"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}, "connection timed out"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "i/o timeout", Name: "shard.example.com", IsTimeout: true}}, "connection timed out"},
		{cut, "connection timed out"},
		{&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, "connection reset by peer"},
		{errors.New("remote error: tls: handshake failure"), "remote error: tls: handshake failure"},
	} {
		if got := client.DialCause(tc.err); got != tc.want {
			t.Errorf("DialCause(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
