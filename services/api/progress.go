package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/presmihaylov/shard/services/image"
)

// ProgressLine is one line of a streamed pull: an event, then the image or the error as the last line.
type ProgressLine struct {
	Event *image.Event `json:"event,omitempty"`
	Image *image.Image `json:"image,omitempty"`
	Error *ErrorObject `json:"error,omitempty"`
}

// CreateLine is one line of a streamed create: a public event, then the sandbox or the error as the last line.
type CreateLine struct {
	Event   *Event       `json:"event,omitempty"`
	Sandbox *Sandbox     `json:"sandbox,omitempty"`
	Error   *ErrorObject `json:"error,omitempty"`
}

// lines builds the lines of one streamed verb: each event as it lands, and the error that ends it after the first.
type lines[L any] struct {
	event  func(image.Event) L
	failed func(ErrorObject) L
}

var pullLines = lines[ProgressLine]{
	event:  func(e image.Event) ProgressLine { return ProgressLine{Event: &e} },
	failed: func(e ErrorObject) ProgressLine { return ProgressLine{Error: &e} },
}

var createLines = lines[CreateLine]{
	event: func(e image.Event) CreateLine {
		pe := publicEvent(e)

		return CreateLine{Event: &pe}
	},
	failed: func(e ErrorObject) CreateLine { return CreateLine{Error: &e} },
}

// streamed says the client asked for the pull's progress as it happens, which only it can print.
func streamed(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), ndjson)
}

// streamProgress writes each event of work as it lands, then its result; an error before the first event keeps its status.
func streamProgress[L any](h *Handler, w http.ResponseWriter, r *http.Request, status int, what string, l lines[L], work func(ctx context.Context) (L, error)) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	type result struct {
		line L
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
		return writeLine(out, l.event(e))
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
		res.line = l.failed(errorBody(res.err).Object)
	}

	if err := writeLine(out, res.line); err != nil {
		h.log.Printf("api: %s: %v", what, err)
	}
}

func writeLine(out *logWriter, line any) error {
	encoded, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("encode a progress line: %w", err)
	}

	if _, err := out.Write(append(encoded, '\n')); err != nil {
		return err
	}

	return nil
}
