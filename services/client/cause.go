package client

import (
	"context"
	"errors"
	"net"
	"syscall"
)

// DialCause words a network failure as a person reads it, never with the socket addresses the dialer puts around it.
func DialCause(err error) string {
	var dns *net.DNSError
	var timeout net.Error
	var errno syscall.Errno
	switch {
	case errors.As(err, &dns) && dns.IsNotFound:
		return "no such host"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return "connection timed out"
	case errors.As(err, &errno):
		return errno.Error()
	}

	return err.Error()
}
