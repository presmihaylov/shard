package api

import (
	"archive/tar"
	"errors"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/presmihaylov/shard/services/sandbox"
)

type archiveInput struct {
	ID   string `path:"id" doc:"The sandbox id or name."`
	Path string `query:"path" doc:"The absolute guest directory."`
	User string `query:"user" doc:"Who unpacks and owns the files; none is the entrypoint's user."`
}

func describeWriteArchive(_ huma.Registry, op *huma.Operation) {
	op.RequestBody = binaryBody("application/x-tar")
	op.Responses["204"] = &huma.Response{Description: "The tar unpacked under the directory."}
}

func describeReadArchive(_ huma.Registry, op *huma.Operation) {
	op.Responses["200"] = &huma.Response{Description: "A tar of the path; a body cut short is a failed read.", Headers: statHeader(), Content: map[string]*huma.MediaType{"application/x-tar": {Schema: binary()}}}
}

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

	watch := watchTrailer()
	_, err = io.Copy(io.MultiWriter(w, watch), body)
	whole, endErr := watch.end()
	if err := errors.Join(err, endErr, body.Close()); err != nil {
		// A client that hangs up once the trailer went out has the whole tar, and its hang-up is what shut the exec (SHARD-410).
		if whole && r.Context().Err() != nil {
			return
		}
		h.log.Printf("api: archive %s from sandbox %s: %v", r.URL.Query().Get("path"), r.PathValue("id"), err)
		// The abort drops the connection without the last chunk, so the client reads a cut, never a whole tar.
		panic(http.ErrAbortHandler)
	}
}

// errStreamEnd ends the watch's copy of a stream, so a tar cut at an entry boundary never reads as one that ended.
var errStreamEnd = errors.New("the archive stream ended")

// trailerWatch reads a copy of what a tar response sent and tells whether its end-of-archive trailer went by.
type trailerWatch struct {
	w    *io.PipeWriter
	seen chan bool
}

func watchTrailer() *trailerWatch {
	r, w := io.Pipe()
	t := &trailerWatch{w: w, seen: make(chan bool, 1)}
	go func() { t.seen <- endsWithTrailer(r) }()

	return t
}

func (t *trailerWatch) Write(p []byte) (int, error) {
	return t.w.Write(p)
}

// end closes the copy and answers whether the trailer went by.
func (t *trailerWatch) end() (bool, error) {
	if err := t.w.CloseWithError(errStreamEnd); err != nil {
		return false, err
	}

	return <-t.seen, nil
}

// endsWithTrailer reads r as a tar up to its trailer, then drains the rest, so the response never waits on the watch.
func endsWithTrailer(r io.Reader) bool {
	whole := readsToTrailer(tar.NewReader(r))
	_, err := io.Copy(io.Discard, r)

	return whole && errors.Is(err, errStreamEnd)
}

func readsToTrailer(tr *tar.Reader) bool {
	for {
		_, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			return false
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return false
		}
	}
}
