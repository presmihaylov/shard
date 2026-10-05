package setup

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// idleTimeout bounds each wait on the network; there is no total timeout, so a slow download that still moves never fails.
const idleTimeout = 30 * time.Second

// newHTTPClient bounds the dial, the TLS handshake, the wait for headers and each wait for body bytes by idle.
func newHTTPClient(idle time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: idle}

	return &http.Client{Transport: &idleTransport{idle: idle, base: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   idle,
		ResponseHeaderTimeout: idle,
	}}}
}

// idleTransport ends a response whose body sends nothing for idle; without it a dropped connection waits out the kernel's retries, about 17 minutes.
type idleTransport struct {
	idle time.Duration
	base http.RoundTripper
}

func (t *idleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()

		return nil, err
	}
	body := &idleBody{ReadCloser: resp.Body, idle: t.idle, cancel: cancel}
	body.timer = time.AfterFunc(t.idle, body.expire)
	resp.Body = body

	return resp, nil
}

// idleBody cancels its request once no byte has arrived for idle, and reports that as a timeout, which reads as "connection timed out".
type idleBody struct {
	io.ReadCloser
	idle    time.Duration
	timer   *time.Timer
	cancel  context.CancelFunc
	expired atomic.Bool
}

func (b *idleBody) expire() {
	b.expired.Store(true)
	b.cancel()
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.expired.Load() {
		return n, &stallError{after: b.idle}
	}
	if n > 0 {
		b.timer.Reset(b.idle)
	}

	return n, err
}

// stallError is a download that sent nothing for after, so setup can say the idle window rather than "connection timed out".
type stallError struct{ after time.Duration }

func (e *stallError) Error() string { return fmt.Sprintf("no data for %s", e.after) }

func (e *stallError) Timeout() bool { return true }

func (e *stallError) Temporary() bool { return false }

// Unwrap keeps errors.Is(err, os.ErrDeadlineExceeded) true, so a caller that tests for a timeout still sees one.
func (e *stallError) Unwrap() error { return os.ErrDeadlineExceeded }

func (b *idleBody) Close() error {
	b.timer.Stop()
	b.cancel()

	return b.ReadCloser.Close()
}
