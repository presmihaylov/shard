package api

import (
	"net/http"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

type snapshotsResponse struct {
	Snapshots []models.Snapshot `json:"snapshots"`
	Next      *string           `json:"next"`
}

func (h *Handler) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var req sandbox.SnapshotRequest
	if err := decode(w, r, &req); err != nil {
		h.writeError(w, err)

		return
	}

	snap, err := h.lifecycle.CreateSnapshot(r.Context(), req)
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusCreated, snap)
}

func (h *Handler) listSnapshots(w http.ResponseWriter, r *http.Request) {
	q, err := pageOf(r, sandboxstate.ValidSnapshotID)
	if err != nil {
		h.writeError(w, err)

		return
	}

	snapshots, err := h.lifecycle.ListSnapshots(r.Context())
	if err != nil {
		h.writeError(w, err)

		return
	}

	snapshots, next := page(snapshots, q, func(snap models.Snapshot) string { return snap.ID })

	h.writeJSON(w, http.StatusOK, snapshotsResponse{Snapshots: snapshots, Next: next})
}

func (h *Handler) getSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := h.lifecycle.InspectSnapshot(r.Context(), r.PathValue("ref"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusOK, snap)
}

func (h *Handler) removeSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := h.lifecycle.RemoveSnapshot(r.Context(), r.PathValue("ref")); err != nil {
		h.writeError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}
