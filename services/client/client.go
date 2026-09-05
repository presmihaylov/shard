// Package client is the typed side of the daemon's REST API, over the unix socket, for the CLI.
package client

import (
	"bytes"
	"context"
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

// Client talks to one daemon. It is safe for concurrent use.
type Client struct {
	path string
	http *http.Client
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
	path := filepath.Join(root, api.SocketFile)

	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err != nil {
			return nil, &ConnectError{Path: path, Err: err}
		}

		return conn, nil
	}}

	return &Client{path: path, http: &http.Client{Transport: transport}, Timeout: DefaultTimeout}
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
		return fmt.Errorf("%s %s on %s: no answer within %s", method, path, c.path, bound)
	}

	return fmt.Errorf("%s %s on %s: %w", method, path, c.path, err)
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
