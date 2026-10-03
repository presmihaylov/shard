package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/image"
)

// ProgressLine is one line of a streamed pull or create: an event, then the result or the error as the last line.
type ProgressLine struct {
	Event   *image.Event    `json:"event,omitempty"`
	Image   *image.Image    `json:"image,omitempty"`
	Sandbox *models.Sandbox `json:"sandbox,omitempty"`
	Error   *ErrorObject    `json:"error,omitempty"`
}

// streamed says the client asked for the pull's progress as it happens, which only it can print.
func streamed(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), ndjson)
}

// streamProgress writes each event of work as it lands, then its result; an error before the first event keeps its status.
func (h *Handler) streamProgress(w http.ResponseWriter, r *http.Request, status int, what string, work func(ctx context.Context) (ProgressLine, error)) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	type result struct {
		line ProgressLine
		err  error
	}
	progress := image.NewProgress()
	done := make(chan result, 1)
	go func() {
		line, err := work(image.WithProgress(ctx, progress))
		progress.Close()
		done <- result{line: line, err: err}
	}()

	out := &logWriter{w: w, contentType: ndjson, status: status}
	followErr := progress.Follow(ctx, func(e image.Event) error {
		return writeLine(out, ProgressLine{Event: &e})
	})
	// A client that cannot take the events is gone, so the work stops the way it does when one hangs up.
	if followErr != nil {
		cancel()
	}
	res := <-done

	if followErr != nil {
		if r.Context().Err() == nil {
			h.log.Printf("api: %s: %v", what, followErr)
		}

		return
	}

	if res.err != nil && !out.wrote {
		h.writeError(w, res.err)

		return
	}
	if res.err != nil {
		_, body := errorBody(res.err)
		res.line = ProgressLine{Error: &body.Error}
	}

	if err := writeLine(out, res.line); err != nil {
		h.log.Printf("api: %s: %v", what, err)
	}
}

func writeLine(out *logWriter, line ProgressLine) error {
	encoded, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("encode a progress line: %w", err)
	}

	if _, err := out.Write(append(encoded, '\n')); err != nil {
		return err
	}

	return nil
}
