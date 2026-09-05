// Package client is the typed side of the daemon's REST API, over the unix socket, for the CLI.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
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
	if err := c.get(ctx, "/v0/version", &out); err != nil {
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
	if err := c.get(ctx, path, &out); err != nil {
		return ListResult{}, err
	}

	return out, nil
}

// GetSandbox answers for an id or a name, with the egress rules the host enforces when the record names a policy.
func (c *Client) GetSandbox(ctx context.Context, ref string) (sandbox.Inspection, error) {
	var out sandbox.Inspection

	err := c.get(ctx, "/v0/sandboxes/"+url.PathEscape(ref), &out)

	var missing *apiError
	if errors.As(err, &missing) && missing.Status == http.StatusNotFound {
		return sandbox.Inspection{}, &NotFoundError{Ref: ref}
	}
	if err != nil {
		return sandbox.Inspection{}, err
	}

	return out, nil
}

// get reads one route into out; the host in the URL is a placeholder, the dialer ignores it.
func (c *Client) get(ctx context.Context, path string, out any) error {
	call := ctx
	if c.Timeout != 0 {
		var cancel context.CancelFunc
		call, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(call, http.MethodGet, "http://shard"+path, nil)
	if err != nil {
		return fmt.Errorf("build the request for %s: %w", path, err)
	}

	resp, err := c.http.Do(req) //nolint:gosec // G704: the ref only lands in the path; the dialer goes to the socket whatever the URL says

	var connect *ConnectError
	if errors.As(err, &connect) {
		return connect
	}
	if err != nil {
		return c.wrap(ctx, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.wrap(ctx, path, fmt.Errorf("read the answer: %w", err))
	}

	if resp.StatusCode != http.StatusOK {
		return decodeError(resp.StatusCode, body)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode the answer to GET %s: %w", path, err)
	}

	return nil
}

// wrap names the route and the socket, and says so when the client's own deadline, not the caller's, cut the call.
func (c *Client) wrap(caller context.Context, path string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && caller.Err() == nil {
		return fmt.Errorf("GET %s on %s: no answer within %s", path, c.path, c.Timeout)
	}

	return fmt.Errorf("GET %s on %s: %w", path, c.path, err)
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
