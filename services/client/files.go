package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

// PutFile streams req.Size bytes of src to the guest path; the daemon answers once the whole file is in place.
func (c *Client) PutFile(ctx context.Context, ref string, req sandbox.FileWrite, src io.Reader) error {
	query := url.Values{"path": {req.Path}, "mode": {strconv.FormatUint(uint64(req.Mode), 8)}}
	if req.User != "" {
		query.Set("user", req.User)
	}
	if req.Parents {
		query.Set("parents", "true")
	}

	resp, err := c.fileRequest(ctx, http.MethodPut, ref, "files", query, src, req.Size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return refusal(resp)
}

// GetFile answers the guest file's stat and its bytes. The caller closes the body; a cut one reads as io.ErrUnexpectedEOF.
func (c *Client) GetFile(ctx context.Context, ref, guestPath string) (models.FileStat, io.ReadCloser, error) {
	resp, err := c.fileRequest(ctx, http.MethodGet, ref, "files", url.Values{"path": {guestPath}}, nil, 0)
	if err != nil {
		return models.FileStat{}, nil, err
	}

	if err := refusal(resp); err != nil {
		return models.FileStat{}, nil, errors.Join(err, resp.Body.Close())
	}
	stat, err := statOf(resp)
	if err != nil {
		return models.FileStat{}, nil, errors.Join(err, resp.Body.Close())
	}

	return stat, resp.Body, nil
}

// StatFile answers the shape of one guest path. A HEAD refusal carries no body, so its error has the status alone.
func (c *Client) StatFile(ctx context.Context, ref, guestPath string) (models.FileStat, error) {
	resp, err := c.fileRequest(ctx, http.MethodHead, ref, "files", url.Values{"path": {guestPath}}, nil, 0)
	if err != nil {
		return models.FileStat{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return models.FileStat{}, &APIError{Status: resp.StatusCode, Code: models.CodeInternal, Message: fmt.Sprintf("the daemon answered %d to a stat of %s in sandbox %s", resp.StatusCode, guestPath, ref)}
	}

	return statOf(resp)
}

// ListDir answers the entries of one guest directory, sorted by name; a listing the guest cut short is an error, never a shorter list.
func (c *Client) ListDir(ctx context.Context, ref, guestPath string) ([]models.FileEntry, error) {
	var out struct {
		Entries []models.FileEntry `json:"entries"`
	}
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/ls?" + url.Values{"path": {guestPath}}.Encode()
	if err := c.call(ctx, http.MethodGet, path, nil, &out, 0); err != nil {
		return nil, err
	}

	return out.Entries, nil
}

// MakeDir makes one guest directory; with req.Parents it makes what leads to it and takes a directory already there.
func (c *Client) MakeDir(ctx context.Context, ref string, req sandbox.MkdirRequest) error {
	return c.call(ctx, http.MethodPost, "/v0/sandboxes/"+url.PathEscape(ref)+"/mkdir", req, nil, 0)
}

// DeleteFile removes one guest path; a directory with anything in it needs recursive.
func (c *Client) DeleteFile(ctx context.Context, ref, guestPath string, recursive bool) error {
	query := url.Values{"path": {guestPath}}
	if recursive {
		query.Set("recursive", "true")
	}

	return c.call(ctx, http.MethodDelete, "/v0/sandboxes/"+url.PathEscape(ref)+"/files?"+query.Encode(), nil, nil, 0)
}

// PutArchive streams the tar src into the guest directory guestPath, as user; the daemon answers once every entry is in place.
func (c *Client) PutArchive(ctx context.Context, ref, guestPath, user string, src io.Reader) error {
	query := url.Values{"path": {guestPath}}
	if user != "" {
		query.Set("user", user)
	}

	resp, err := c.fileRequest(ctx, http.MethodPut, ref, "archive", query, src, -1)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return refusal(resp)
}

// GetArchive answers the guest path's stat and a tar of it, whose top entry is the path's base name. The caller closes the body.
func (c *Client) GetArchive(ctx context.Context, ref, guestPath string) (models.FileStat, io.ReadCloser, error) {
	resp, err := c.fileRequest(ctx, http.MethodGet, ref, "archive", url.Values{"path": {guestPath}}, nil, 0)
	if err != nil {
		return models.FileStat{}, nil, err
	}

	if err := refusal(resp); err != nil {
		return models.FileStat{}, nil, errors.Join(err, resp.Body.Close())
	}
	stat, err := statOf(resp)
	if err != nil {
		return models.FileStat{}, nil, errors.Join(err, resp.Body.Close())
	}

	return stat, resp.Body, nil
}

// fileRequest sends one /files or /archive call with no deadline of its own, since a copy streams for as long as it takes; a size of -1 sends the body chunked.
func (c *Client) fileRequest(ctx context.Context, method, ref, route string, query url.Values, body io.Reader, size int64) (*http.Response, error) {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/" + route
	switch {
	// net/http reads a zero length with a body as unknown and sends it chunked, which the daemon refuses.
	case body != nil && size == 0:
		body = http.NoBody
	// The transport closes a body that is a Closer, and the caller's file is the caller's to close.
	case body != nil:
		body = io.NopCloser(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint("http", path+"?"+query.Encode()), body) //nolint:gosec // G704: the ref only lands in the path; the dialer goes to the socket whatever the URL says
	if err != nil {
		return nil, fmt.Errorf("build the request for %s %s: %w", method, path, err)
	}
	// An *os.File gives net/http no length, and the daemon needs it: the guest takes exactly that many bytes.
	if body != nil {
		req.ContentLength = size
	}
	c.authorize(req.Header)

	resp, err := c.http.Do(req) //nolint:gosec // G704: the ref only lands in the path; the dialer goes to the socket whatever the URL says

	var connect *ConnectError
	if errors.As(err, &connect) {
		return nil, connect
	}
	if err != nil {
		return nil, fmt.Errorf("%s %s on %s: %w", method, path, c.target, unquoted(err))
	}

	return resp, nil
}

func refusal(resp *http.Response) error {
	if resp.StatusCode < http.StatusBadRequest {
		return nil
	}

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read the daemon's refusal: %w", err)
	}

	return decodeError(resp.StatusCode, answer)
}

func statOf(resp *http.Response) (models.FileStat, error) {
	var stat models.FileStat
	if err := json.Unmarshal([]byte(resp.Header.Get(api.StatHeader)), &stat); err != nil {
		return models.FileStat{}, fmt.Errorf("decode the %s header %q: %w", api.StatHeader, resp.Header.Get(api.StatHeader), err)
	}

	return stat, nil
}
