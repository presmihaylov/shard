package api

import (
	"net/http"

	"github.com/presmihaylov/shard/services/sandbox"
)

// notImplementedError is a route that holds its final shape before the ticket behind it lands.
type notImplementedError struct{ what, ticket string }

func (e *notImplementedError) Error() string {
	return e.what + ": not implemented yet (" + e.ticket + ")"
}

// snapshotsTicket is the work that fills in the snapshot routes and create from a snapshot.
const snapshotsTicket = "SHARD-457a"

func (h *Handler) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var req sandbox.SnapshotRequest
	if err := decode(w, r, &req); err != nil {
		h.writeError(w, err)

		return
	}

	h.writeError(w, &notImplementedError{what: "snapshot create", ticket: snapshotsTicket})
}

func (h *Handler) listSnapshots(w http.ResponseWriter, _ *http.Request) {
	h.writeError(w, &notImplementedError{what: "snapshot list", ticket: snapshotsTicket})
}

func (h *Handler) getSnapshot(w http.ResponseWriter, _ *http.Request) {
	h.writeError(w, &notImplementedError{what: "snapshot inspect", ticket: snapshotsTicket})
}

func (h *Handler) removeSnapshot(w http.ResponseWriter, _ *http.Request) {
	h.writeError(w, &notImplementedError{what: "snapshot remove", ticket: snapshotsTicket})
}
