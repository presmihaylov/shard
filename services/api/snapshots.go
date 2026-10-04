package api

import (
	"context"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

type snapshotsResponse struct {
	Snapshots []models.Snapshot `json:"snapshots"`
	Next      *string           `json:"next"`
}

func (h *Handler) createSnapshot(ctx context.Context, in *bodyInput[sandbox.SnapshotRequest]) (*reply[models.Snapshot], error) {
	return answer(h.lifecycle.CreateSnapshot(ctx, value(in.Body)))
}

func (h *Handler) listSnapshots(ctx context.Context, in *pageInput) (*reply[snapshotsResponse], error) {
	q, err := paged(in.Limit, in.Cursor, sandboxstate.ValidSnapshotID)
	if err != nil {
		return nil, fail(err)
	}

	snapshots, err := h.lifecycle.ListSnapshots(ctx)
	if err != nil {
		return nil, fail(err)
	}

	snapshots, next := page(snapshots, q, func(snap models.Snapshot) string { return snap.ID })

	return answer(snapshotsResponse{Snapshots: snapshots, Next: next}, nil)
}

func (h *Handler) getSnapshot(ctx context.Context, in *refPath) (*reply[models.Snapshot], error) {
	return answer(h.lifecycle.InspectSnapshot(ctx, in.Ref))
}

func (h *Handler) removeSnapshot(ctx context.Context, in *refPath) (*struct{}, error) {
	return done(h.lifecycle.RemoveSnapshot(ctx, in.Ref))
}
