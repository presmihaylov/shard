// Package splice copies bytes both ways between two connections and passes a half close across where the far side takes one.
package splice

import (
	"errors"
	"io"
	"net"
	"syscall"
)

// Conns copies a to b and b to a until both directions end, then closes both; an error in either direction closes both at once.
func Conns(a, b io.ReadWriteCloser) error {
	ended := make(chan error, 2)
	go func() { ended <- half(b, a) }()
	go func() { ended <- half(a, b) }()

	first := <-ended
	if first != nil {
		closed := closeBoth(a, b)

		return errors.Join(quietOr(first), quietOr(<-ended), closed)
	}

	return errors.Join(quietOr(<-ended), closeBoth(a, b))
}

// errNoHalfClose ends a direction whose far side cannot take a half close, which closes both.
var errNoHalfClose = errors.New("the far side takes no half close")

// half copies src into dst and, once src ends cleanly, closes dst for writing.
func half(dst, src io.ReadWriteCloser) error {
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	closer, ok := dst.(interface{ CloseWrite() error })
	if !ok {
		return errNoHalfClose
	}

	return closer.CloseWrite()
}

func closeBoth(a, b io.Closer) error {
	return errors.Join(quietOr(a.Close()), quietOr(b.Close()))
}

// quietOr drops the errors a connection's ordinary end raises: either peer closing, resetting or going away first.
func quietOr(err error) error {
	if quiet(err) {
		return nil
	}

	return err
}

func quiet(err error) bool {
	return err == nil ||
		errors.Is(err, errNoHalfClose) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ENOTCONN)
}
