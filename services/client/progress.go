package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandbox"
)

// PullEvent is one step of a pull, as the daemon streams it while the image lands.
type PullEvent = image.Event

// The statuses a PullEvent carries, in the order a pull says them.
const (
	PullCached  = image.StatusCached
	PullPulling = image.StatusPulling
	PullLayer   = image.StatusLayer
	PullPulled  = image.StatusPulled
)

// CreateSandboxAndWait is one call, so a pull that ends before a separate wait could attach is never missed.
func (c *Client) CreateSandboxAndWait(ctx context.Context, req sandbox.CreateRequest, report func(PullEvent)) (models.Sandbox, error) {
	return progress(ctx, c, "/v0/sandboxes?wait=true", req, report, func(line api.ProgressLine) *models.Sandbox { return line.Sandbox })
}

// progress hands each streamed event to report and answers the last line; an older daemon answers plain JSON, read whole.
func progress[T any](ctx context.Context, c *Client, path string, in any, report func(PullEvent), pick func(api.ProgressLine) *T) (T, error) {
	var zero T

	encoded, err := json.Marshal(in)
	if err != nil {
		return zero, fmt.Errorf("encode the request for POST %s: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://shard"+path, bytes.NewReader(encoded))
	if err != nil {
		return zero, fmt.Errorf("build the request for POST %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	c.authorize(req.Header)

	resp, err := c.http.Do(req) //nolint:gosec // G704: the dialer goes to the socket whatever the URL says

	var connect *ConnectError
	if errors.As(err, &connect) {
		return zero, connect
	}
	if err != nil {
		return zero, c.wrap(ctx, http.MethodPost, path, 0, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return zero, c.wrap(ctx, http.MethodPost, path, 0, fmt.Errorf("read the answer: %w", err))
		}

		return zero, decodeError(resp.StatusCode, body)
	}

	decoder := json.NewDecoder(resp.Body)
	if media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); media != "application/x-ndjson" {
		var out T
		if err := decoder.Decode(&out); err != nil {
			return zero, fmt.Errorf("decode the answer to POST %s: %w", path, err)
		}

		return out, nil
	}

	for {
		var line api.ProgressLine
		if err := decoder.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				return zero, fmt.Errorf("POST %s on %s: the daemon ended the answer before the result", path, c.target)
			}

			return zero, c.wrap(ctx, http.MethodPost, path, 0, fmt.Errorf("read the answer: %w", err))
		}

		if line.Event != nil {
			if report != nil {
				report(*line.Event)
			}

			continue
		}

		// The answer is already a 2xx, so a failure after the first event has a code and no status.
		if line.Error != nil {
			return zero, &APIError{Code: line.Error.Code, Message: line.Error.Message, Holders: line.Error.Holders}
		}

		result := pick(line)
		if result == nil {
			return zero, fmt.Errorf("POST %s on %s: the daemon answered no result", path, c.target)
		}

		return *result, nil
	}
}
