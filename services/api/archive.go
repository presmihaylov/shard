package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/presmihaylov/shard/services/sandbox"
)

// putArchive unpacks the tar body under the guest directory at ?path=; the body streams, so no length is needed, and a stall still ends it.
func (h *Handler) putArchive(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	req := sandbox.ArchiveWrite{Path: query.Get("path"), User: query.Get("user")}
	if err := h.lifecycle.WriteArchive(r.Context(), r.PathValue("id"), req, idleBounded(w, r)); err != nil {
		h.writeError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// getArchive streams a tar of the guest path at ?path=, its stat up front; a tar can end cleanly at any entry, so a cut aborts the response.
func (h *Handler) getArchive(w http.ResponseWriter, r *http.Request) {
	stat, body, err := h.lifecycle.ReadArchive(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	if err := setStat(w, stat); err != nil {
		h.writeError(w, errors.Join(err, body.Close()))

		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)

	_, err = io.Copy(w, body)
	if err := errors.Join(err, body.Close()); err != nil {
		h.log.Printf("api: archive %s from sandbox %s: %v", r.URL.Query().Get("path"), r.PathValue("id"), err)
		// The abort drops the connection without the last chunk, so the client reads a cut, never a whole tar.
		panic(http.ErrAbortHandler)
	}
}
