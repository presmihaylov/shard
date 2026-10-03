package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// StatHeader carries a guest path's models.FileStat as JSON, on a HEAD and on a GET of /files.
const StatHeader = "X-Shard-Stat"

// putFile lands the body at ?path= as one file; the length must be known, since the guest takes exactly that many bytes.
func (h *Handler) putFile(w http.ResponseWriter, r *http.Request) {
	req, err := fileWriteOf(r)
	if err != nil {
		h.writeError(w, err)

		return
	}

	if err := h.lifecycle.WriteFile(r.Context(), r.PathValue("id"), req, idleBounded(w, r)); err != nil {
		h.writeError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// idleBounded turns the server's ReadTimeout into an idle bound, so a large body that keeps moving is never cut and one that stalls still is.
func idleBounded(w http.ResponseWriter, r *http.Request) io.Reader {
	server, ok := r.Context().Value(http.ServerContextKey).(*http.Server)
	if !ok || server.ReadTimeout <= 0 {
		return r.Body
	}

	return &idleBody{body: r.Body, control: http.NewResponseController(w), idle: server.ReadTimeout}
}

// idleBody moves the read deadline before each read and stops at the end, since net/http clears it then for its own background read.
type idleBody struct {
	body    io.Reader
	control *http.ResponseController
	idle    time.Duration
	ended   bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	if b.ended {
		return 0, io.EOF
	}
	if err := b.control.SetReadDeadline(time.Now().Add(b.idle)); err != nil {
		return 0, fmt.Errorf("move the body's read deadline: %w", err)
	}

	n, err := b.body.Read(p)
	if errors.Is(err, io.EOF) {
		b.ended = true
	}

	return n, err
}

func fileWriteOf(r *http.Request) (sandbox.FileWrite, error) {
	if r.ContentLength < 0 {
		return sandbox.FileWrite{}, &sandbox.RequestError{Err: errors.New("a put needs a Content-Length: the guest lands exactly that many bytes")}
	}

	mode := uint64(sandbox.DefaultFileMode)
	if raw := r.URL.Query().Get("mode"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 8, 32)
		if err != nil {
			return sandbox.FileWrite{}, &sandbox.RequestError{Err: fmt.Errorf("the query mode=%q is not an octal mode", raw)}
		}
		mode = parsed
	}

	parents, err := boolQuery(r, "parents")
	if err != nil {
		return sandbox.FileWrite{}, err
	}

	query := r.URL.Query()

	return sandbox.FileWrite{Path: query.Get("path"), Mode: uint32(mode), User: query.Get("user"), Parents: parents, Size: r.ContentLength}, nil
}

// getFile streams the guest file at ?path= with its stat up front and chunked to its end, since a /proc file's size is not its length.
func (h *Handler) getFile(w http.ResponseWriter, r *http.Request) {
	stat, body, err := h.lifecycle.ReadFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	if err := setStat(w, stat); err != nil {
		h.writeError(w, errors.Join(err, body.Close()))

		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)

	// The stat goes out before the bytes, so a guest that fails later cuts a chunked body rather than dropping the whole answer.
	flushErr := http.NewResponseController(w).Flush()
	_, err = io.Copy(w, body)
	if err := errors.Join(flushErr, err, body.Close()); err != nil {
		h.log.Printf("api: get %s from sandbox %s: %v", r.URL.Query().Get("path"), r.PathValue("id"), err)
		// The 200 is out, so only a body cut before its last chunk tells the client the file is short.
		panic(http.ErrAbortHandler)
	}
}

// statFile answers the shape of the guest path at ?path= in a header, with no body.
func (h *Handler) statFile(w http.ResponseWriter, r *http.Request) {
	stat, err := h.lifecycle.StatFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	if err := setStat(w, stat); err != nil {
		h.writeError(w, err)

		return
	}
	w.WriteHeader(http.StatusOK)
}

func setStat(w http.ResponseWriter, stat models.FileStat) error {
	encoded, err := json.Marshal(stat)
	if err != nil {
		return fmt.Errorf("encode the stat: %w", err)
	}
	w.Header().Set(StatHeader, string(encoded))

	return nil
}

// listDir streams the entries of the guest directory at ?path=, so no listing has to fit in the daemon's memory.
func (h *Handler) listDir(w http.ResponseWriter, r *http.Request) {
	listing, err := h.lifecycle.ListDir(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// The 200 is out, so a failure now goes to the daemon's log; the body then lacks its closing bracket, which no client parses.
	if err := writeEntries(w, listing); err != nil {
		h.log.Printf("api: ls %s in sandbox %s: %v", r.URL.Query().Get("path"), r.PathValue("id"), err)
	}
}

// writeEntries closes the JSON only once the guest has said it sent every entry, so a cut listing never parses as a whole one.
func writeEntries(w io.Writer, listing sandbox.Listing) error {
	if _, err := io.WriteString(w, `{"entries":[`); err != nil {
		return errors.Join(fmt.Errorf("write the listing: %w", err), listing.Close())
	}
	for sep := ""; ; sep = "," {
		entry, err := listing.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.Join(err, listing.Close())
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return errors.Join(fmt.Errorf("encode the entry %s: %w", entry.Name, err), listing.Close())
		}
		if _, err := io.WriteString(w, sep+string(encoded)); err != nil {
			return errors.Join(fmt.Errorf("write the listing: %w", err), listing.Close())
		}
	}
	if err := listing.Close(); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "]}"); err != nil {
		return fmt.Errorf("write the listing: %w", err)
	}

	return nil
}

// makeDir makes the directory the JSON body names.
func (h *Handler) makeDir(w http.ResponseWriter, r *http.Request) {
	var req sandbox.MkdirRequest
	if err := decode(w, r, &req); err != nil {
		h.writeError(w, err)

		return
	}

	if err := h.lifecycle.MakeDir(r.Context(), r.PathValue("id"), req); err != nil {
		h.writeError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// deleteFile removes the guest path at ?path=; ?recursive=true takes a directory and everything in it.
func (h *Handler) deleteFile(w http.ResponseWriter, r *http.Request) {
	recursive, err := boolQuery(r, "recursive")
	if err != nil {
		h.writeError(w, err)

		return
	}

	if err := h.lifecycle.DeleteFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"), recursive); err != nil {
		h.writeError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}
