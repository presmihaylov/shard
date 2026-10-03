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

	resp, err := c.fileRequest(ctx, http.MethodPut, ref, query, src, req.Size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return refusal(resp)
}

// GetFile answers the guest file's stat and its bytes. The caller closes the body; a cut one reads as io.ErrUnexpectedEOF.
func (c *Client) GetFile(ctx context.Context, ref, guestPath string) (models.FileStat, io.ReadCloser, error) {
	resp, err := c.fileRequest(ctx, http.MethodGet, ref, url.Values{"path": {guestPath}}, nil, 0)
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
	resp, err := c.fileRequest(ctx, http.MethodHead, ref, url.Values{"path": {guestPath}}, nil, 0)
	if err != nil {
		return models.FileStat{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return models.FileStat{}, &APIError{Status: resp.StatusCode, Code: models.CodeInternal, Message: fmt.Sprintf("the daemon answered %d to a stat of %s in sandbox %s", resp.StatusCode, guestPath, ref)}
	}

	return statOf(resp)
}

// fileRequest sends one /files call with no deadline of its own, since a file streams for as long as it takes.
func (c *Client) fileRequest(ctx context.Context, method, ref string, query url.Values, body io.Reader, size int64) (*http.Response, error) {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/files"
	switch {
	// net/http reads a zero length with a body as unknown and sends it chunked, which the daemon refuses.
	case body != nil && size == 0:
		body = http.NoBody
	// The transport closes a body that is a Closer, and the caller's file is the caller's to close.
	case body != nil:
		body = io.NopCloser(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://shard"+path+"?"+query.Encode(), body) //nolint:gosec // G704: the ref only lands in the path; the dialer goes to the socket whatever the URL says
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
		return nil, fmt.Errorf("%s %s on %s: %w", method, path, c.target, err)
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
