package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
)

// AttachApp writes what the app of a run writes to w until its restart policy ends, and answers how it ended.
func (c *Client) AttachApp(ctx context.Context, ref string, w io.Writer) (exit models.AppExit, err error) {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/attach"

	conn, err := c.open(ctx, path, "the main command of sandbox "+ref)
	if err != nil {
		return models.AppExit{}, missing(ref, err)
	}
	defer func() { err = errors.Join(err, closeStream(conn, err)) }()

	for {
		stream, payload, err := api.Receive(ctx, conn)
		if err != nil {
			return models.AppExit{}, dropped("attach to the main command of sandbox "+ref, err)
		}

		switch stream {
		case api.StreamStdout:
			if err := write(w, payload); err != nil {
				return models.AppExit{}, err
			}
		case api.StreamExit:
			if err := json.Unmarshal(payload, &exit); err != nil {
				return models.AppExit{}, fmt.Errorf("the daemon answered %q as how the main command of sandbox %s ended", payload, ref)
			}

			return exit, nil
		case api.StreamFailure:
			return models.AppExit{}, failureOf(payload, ref)
		default:
			return models.AppExit{}, fmt.Errorf("the daemon sent an unknown stream %d; upgrade shard to match the daemon", stream)
		}
	}
}

// StopApp cancels every start again of the app and terms it, or kills it with force; the sandbox stays running.
func (c *Client) StopApp(ctx context.Context, ref string, force bool) error {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/app/stop"

	req := struct {
		Force bool `json:"force,omitempty"`
	}{Force: force}

	if err := c.call(ctx, http.MethodPost, path, req, nil, c.Timeout); err != nil {
		return missing(ref, err)
	}

	return nil
}
