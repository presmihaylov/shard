package api

import (
	"context"
	"io"
	"net/http"

	"github.com/coder/websocket"
	"github.com/danielgtaylor/huma/v2"

	"github.com/presmihaylov/shard/models"
)

// attachApp answers how a run's app ended: a WebSocket upgrade streams its output first, and a plain request answers the exit alone.
func (h *Handler) attachApp(w http.ResponseWriter, r *http.Request) {
	if isHandshake(r) {
		h.streamApp(w, r)

		return
	}

	exit, err := h.lifecycle.WaitApp(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	h.writeJSON(w, http.StatusOK, exit)
}

// streamApp sends the app's output on stream 1 from the start of the log, then one exit on stream 3; every refusal comes before the 101.
func (h *Handler) streamApp(w http.ResponseWriter, r *http.Request) {
	// The poll runs under ctx, so a client that hangs up ends it even while the app writes nothing.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	answered := false
	var f *follower
	exit, err := h.lifecycle.AttachApp(ctx, r.PathValue("id"), func() (io.Writer, error) {
		answered = true
		opened, err := h.follow(w, r.WithContext(ctx), "app of sandbox "+r.PathValue("id"))
		if err != nil {
			return nil, err
		}
		f = opened
		context.AfterFunc(f.ctx, cancel)

		return writerFunc(func(p []byte) (int, error) {
			if err := Send(f.ctx, f.conn, StreamStdout, p); err != nil {
				return 0, err
			}

			return len(p), nil
		}), nil
	})

	// Nothing was said on the wire yet, so the refusal is a status and a JSON body like every other route.
	if !answered {
		h.writeError(w, r, err)

		return
	}
	// The library answered the handshake with its own refusal, so the daemon's log is the one place left.
	if f == nil {
		h.log.Printf("api: app of sandbox %s: %v", r.PathValue("id"), err)

		return
	}
	defer f.close(websocket.StatusNormalClosure, "")

	// The client hung up or the daemon is going down, and neither is anything to say on the wire.
	if f.ctx.Err() != nil {
		return
	}
	if err != nil {
		f.send(StreamFailure, h.failureOf(r, err))

		return
	}

	f.send(StreamExit, exit)
}

// appStopRequest says how a stop ends the app: TERM, or KILL with force.
type appStopRequest struct {
	Force bool `json:"force,omitempty"`
}

// stopApp cancels every start again of the app and ends it; the sandbox stays running.
func (h *Handler) stopApp(ctx context.Context, in *sandboxBody[appStopRequest]) (*struct{}, error) {
	return done(h.lifecycle.StopApp(ctx, in.ID, value(in.Body).Force))
}

// describeAttachApp names the two answers of attachApp: the exit, or the output and then the exit over a WebSocket.
func describeAttachApp(registry huma.Registry, op *huma.Operation) {
	op.Responses["200"] = response("How the app ended, once it ends.", "application/json", schemaOf[models.AppExit](registry))
	op.Responses["101"] = upgrade("A WebSocket attach. Each binary message leads with its stream byte: 1 the output from the start of the log, 3 the AppExit, 5 a FailureMessage.")
}
