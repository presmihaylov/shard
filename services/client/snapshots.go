package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

type snapshotsResult struct {
	Snapshots []models.Snapshot `json:"snapshots"`
}

// CreateSnapshot takes as long as the files it copies, so no per-request deadline bounds it.
func (c *Client) CreateSnapshot(ctx context.Context, req sandbox.SnapshotRequest) (models.Snapshot, error) {
	var out models.Snapshot
	if err := c.call(ctx, http.MethodPost, "/v0/snapshots", req, &out, 0); err != nil {
		return models.Snapshot{}, missing(req.Sandbox, err)
	}

	return out, nil
}

func (c *Client) ListSnapshots(ctx context.Context) ([]models.Snapshot, error) {
	var out snapshotsResult
	if err := c.call(ctx, http.MethodGet, "/v0/snapshots", nil, &out, c.Timeout); err != nil {
		return nil, err
	}

	return out.Snapshots, nil
}

// InspectSnapshot leaves a not_found as the daemon wrote it: NotFoundError names a sandbox.
func (c *Client) InspectSnapshot(ctx context.Context, ref string) (models.Snapshot, error) {
	var out models.Snapshot
	if err := c.call(ctx, http.MethodGet, "/v0/snapshots/"+url.PathEscape(ref), nil, &out, c.Timeout); err != nil {
		return models.Snapshot{}, err
	}

	return out, nil
}

// RemoveSnapshot deletes as many files as the copy holds, so no per-request deadline bounds it.
func (c *Client) RemoveSnapshot(ctx context.Context, ref string) error {
	return c.call(ctx, http.MethodDelete, "/v0/snapshots/"+url.PathEscape(ref), nil, nil, 0)
}
