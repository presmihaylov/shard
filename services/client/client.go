// Package client is the typed side of the daemon's REST API, for the CLI. It speaks the unix
// socket, or the same routes over tls to a shard serve front.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

// DefaultTimeout bounds one call that answers in full. A call that streams passes zero.
const DefaultTimeout = 30 * time.Second

// remotePort is the port a --host with none named is dialed on, which is what shard serve binds.
const remotePort = "2376"

// Client talks to one daemon. It is safe for concurrent use.
type Client struct {
	// target is the socket path or the host url, which is what an error names.
	target string
	// dialer is the whole of the transport switch: the unix socket, or tls to a shard serve front.
	dialer func(ctx context.Context) (net.Conn, error)
	// token is the bearer token a front checks. The socket takes none: its mode is the check.
	token string
	http  *http.Client
	// Timeout bounds one call. It is not http.Client.Timeout, which would cut a stream; zero is no bound.
	Timeout time.Duration
}

// Version is what the daemon reports for itself.
type Version struct {
	Version string `json:"version"`
}

// ListResult is what ls prints: the sandboxes the daemon read, beside the records it could not.
type ListResult struct {
	Sandboxes []models.Sandbox `json:"sandboxes"`
	Warnings  []string         `json:"warnings,omitempty"`
}

// ConnectError is a socket nothing answers on. Its text is the one line the operator needs.
type ConnectError struct {
	Path string
	Err  error
}

func (e *ConnectError) Error() string {
	return fmt.Sprintf("cannot connect to shard daemon at %s: is it running? systemctl status shard", e.Path)
}

func (e *ConnectError) Unwrap() error { return e.Err }

// NotFoundError is the daemon's 404: nothing holds the reference.
type NotFoundError struct {
	Ref string
}

func (e *NotFoundError) Error() string { return "no sandbox " + e.Ref }

// apiError is any other status the daemon answered with, carrying its message.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return e.Message }

// New dials the socket under root on the first request.
func New(root string) *Client {
	socket := filepath.Join(root, api.SocketFile)

	c := &Client{target: socket, Timeout: DefaultTimeout}
	c.dialer = func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	c.transport()

	return c
}

// NewRemote dials a shard serve front over TLS instead, with the token every request to it carries.
// The routes and the framing are the same: the front is a byte proxy onto that same socket.
func NewRemote(host, token string, ca []byte) (*Client, error) {
	parsed, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("parse the host %q: %w", host, err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("--host must be an https url, as https://box.example.com:2376, got %q", host)
	}
	if token == "" {
		return nil, errors.New("--host needs a token: a shard serve front answers 401 without one")
	}

	address := parsed.Host
	if parsed.Port() == "" {
		address = net.JoinHostPort(parsed.Hostname(), remotePort)
	}

	settings := &tls.Config{ServerName: parsed.Hostname(), MinVersion: tls.VersionTLS12}
	if len(ca) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("the ca file holds no certificate")
		}
		settings.RootCAs = pool
	}

	c := &Client{target: host, token: token, Timeout: DefaultTimeout}
	c.dialer = func(ctx context.Context) (net.Conn, error) {
		return (&tls.Dialer{Config: settings}).DialContext(ctx, "tcp", address)
	}
	c.transport()

	return c, nil
}

// transport sends every request that net/http builds over this client's own dialer.
func (c *Client) transport() {
	c.http = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return c.dial(ctx)
	}}}
}

// dial opens the connection. An exec dials it itself, because it takes the connection over from HTTP.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	conn, err := c.dialer(ctx)

	// A certificate the client does not trust is its own error: nothing about the daemon is wrong.
	var untrusted *tls.CertificateVerificationError
	if errors.As(err, &untrusted) {
		return nil, fmt.Errorf("the tls certificate of %s is not trusted: %w", c.target, err)
	}
	if err != nil {
		return nil, &ConnectError{Path: c.target, Err: err}
	}

	return conn, nil
}

// authorize carries the bearer token of a front. The socket takes none: its mode is the check.
func (c *Client) authorize(req *http.Request) {
	if c.token == "" {
		return
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
}

func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	if err := c.call(ctx, http.MethodGet, "/v0/version", nil, &out, c.Timeout); err != nil {
		return Version{}, err
	}

	return out, nil
}

func (c *Client) ListSandboxes(ctx context.Context, all bool) (ListResult, error) {
	path := "/v0/sandboxes"
	if all {
		path += "?all=true"
	}

	var out ListResult
	if err := c.call(ctx, http.MethodGet, path, nil, &out, c.Timeout); err != nil {
		return ListResult{}, err
	}

	return out, nil
}

// GetSandbox answers for an id or a name, with the egress rules the host enforces when the record names a policy.
func (c *Client) GetSandbox(ctx context.Context, ref string) (sandbox.Inspection, error) {
	var out sandbox.Inspection
	if err := c.call(ctx, http.MethodGet, "/v0/sandboxes/"+url.PathEscape(ref), nil, &out, c.Timeout); err != nil {
		return sandbox.Inspection{}, missing(ref, err)
	}

	return out, nil
}

// CreateSandbox pulls the image inside the daemon, so the call has no bound of its own: only the caller's context ends it.
func (c *Client) CreateSandbox(ctx context.Context, req sandbox.CreateRequest) (models.Sandbox, error) {
	var out models.Sandbox
	if err := c.call(ctx, http.MethodPost, "/v0/sandboxes", req, &out, 0); err != nil {
		return models.Sandbox{}, err
	}

	return out, nil
}

func (c *Client) StartSandbox(ctx context.Context, ref string) (models.Sandbox, error) {
	var out models.Sandbox
	if err := c.call(ctx, http.MethodPost, "/v0/sandboxes/"+url.PathEscape(ref)+"/start", nil, &out, c.Timeout); err != nil {
		return models.Sandbox{}, missing(ref, err)
	}

	return out, nil
}

// StopSandbox waits the grace on top of the usual bound, because the daemon does before it answers.
func (c *Client) StopSandbox(ctx context.Context, ref string, grace time.Duration) (models.Sandbox, error) {
	body := struct {
		Grace float64 `json:"grace"`
	}{Grace: grace.Seconds()}

	var out models.Sandbox
	if err := c.call(ctx, http.MethodPost, "/v0/sandboxes/"+url.PathEscape(ref)+"/stop", body, &out, c.plus(grace)); err != nil {
		return models.Sandbox{}, missing(ref, err)
	}

	return out, nil
}

// RemoveSandbox frees a stopped sandbox; force stops a live one first, with grace as that stop's.
func (c *Client) RemoveSandbox(ctx context.Context, ref string, force bool, grace time.Duration) error {
	path := "/v0/sandboxes/" + url.PathEscape(ref)
	if force {
		path += "?force=true&grace=" + strconv.FormatFloat(grace.Seconds(), 'f', -1, 64)
	}

	if err := c.call(ctx, http.MethodDelete, path, nil, nil, c.plus(grace)); err != nil {
		return missing(ref, err)
	}

	return nil
}

// PauseSandbox has no bound of its own: a checkpoint takes as long as the memory and the disk it writes.
func (c *Client) PauseSandbox(ctx context.Context, ref string) (models.Sandbox, error) {
	var out models.Sandbox
	if err := c.call(ctx, http.MethodPost, "/v0/sandboxes/"+url.PathEscape(ref)+"/pause", nil, &out, 0); err != nil {
		return models.Sandbox{}, missing(ref, err)
	}

	return out, nil
}

// ResumeSandbox reads back what the pause wrote, so it takes no bound either.
func (c *Client) ResumeSandbox(ctx context.Context, ref string) (models.Sandbox, error) {
	var out models.Sandbox
	if err := c.call(ctx, http.MethodPost, "/v0/sandboxes/"+url.PathEscape(ref)+"/resume", nil, &out, 0); err != nil {
		return models.Sandbox{}, missing(ref, err)
	}

	return out, nil
}

// ForkSandbox starts a second sandbox from the source's snapshot.
func (c *Client) ForkSandbox(ctx context.Context, ref string, req sandbox.CopyRequest) (models.Sandbox, error) {
	return c.copy(ctx, ref, "/fork", req)
}

// CloneSandbox copies a stopped or paused sandbox's disk into a new one.
func (c *Client) CloneSandbox(ctx context.Context, ref string, req sandbox.CopyRequest) (models.Sandbox, error) {
	return c.copy(ctx, ref, "/clone", req)
}

func (c *Client) copy(ctx context.Context, ref, verb string, req sandbox.CopyRequest) (models.Sandbox, error) {
	var out models.Sandbox
	if err := c.call(ctx, http.MethodPost, "/v0/sandboxes/"+url.PathEscape(ref)+verb, req, &out, 0); err != nil {
		return models.Sandbox{}, missing(ref, err)
	}

	return out, nil
}

// plus stretches the bound by what the daemon itself waits for; no bound stays no bound.
func (c *Client) plus(grace time.Duration) time.Duration {
	if c.Timeout == 0 {
		return 0
	}

	return c.Timeout + grace
}

// missing turns the daemon's 404 into the one error a verb prints as its own line.
func missing(ref string, err error) error {
	var answer *apiError
	if errors.As(err, &answer) && answer.Status == http.StatusNotFound {
		return &NotFoundError{Ref: ref}
	}

	return err
}

// call sends in as JSON and decodes out, each when set, under bound; zero is no deadline.
func (c *Client) call(ctx context.Context, method, path string, in, out any, bound time.Duration) error {
	call := ctx
	if bound != 0 {
		var cancel context.CancelFunc
		call, cancel = context.WithTimeout(ctx, bound)
		defer cancel()
	}

	var payload io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode the request for %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(call, method, "http://shard"+path, payload)
	if err != nil {
		return fmt.Errorf("build the request for %s %s: %w", method, path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req)

	resp, err := c.http.Do(req) //nolint:gosec // G704: the ref only lands in the path; the dialer goes to the socket whatever the URL says

	var connect *ConnectError
	if errors.As(err, &connect) {
		return connect
	}
	if err != nil {
		return c.wrap(ctx, method, path, bound, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.wrap(ctx, method, path, bound, fmt.Errorf("read the answer: %w", err))
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return decodeError(resp.StatusCode, body)
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode the answer to %s %s: %w", method, path, err)
	}

	return nil
}

// wrap names the route and the socket, and says so when the client's own deadline, not the caller's, cut the call.
func (c *Client) wrap(caller context.Context, method, path string, bound time.Duration, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && caller.Err() == nil {
		return fmt.Errorf("%s %s on %s: no answer within %s", method, path, c.target, bound)
	}

	return fmt.Errorf("%s %s on %s: %w", method, path, c.target, err)
}

// decodeError reads the daemon's error object; a body that is not one is quoted as it came.
func decodeError(status int, body []byte) error {
	var answer struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Error == "" {
		return &apiError{Status: status, Message: fmt.Sprintf("the daemon answered %d: %q", status, body)}
	}

	return &apiError{Status: status, Message: answer.Error}
}
