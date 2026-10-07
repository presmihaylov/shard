package api

import (
	"context"
	"io"
	"net/http"

	"github.com/coder/websocket"
	"github.com/danielgtaylor/huma/v2"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

type processPath struct {
	ID   string `path:"id" doc:"The sandbox id or name."`
	Name string `path:"name" doc:"The process name."`
}

type processBody[B any] struct {
	ID   string `path:"id" doc:"The sandbox id or name."`
	Name string `path:"name" doc:"The process name."`
	Body *B
}

type processLogsInput struct {
	ID     string `path:"id" doc:"The sandbox id or name."`
	Name   string `path:"name" doc:"The process name."`
	Follow bool   `query:"follow" doc:"Keep the stream open until the process ends or the sandbox stops or is removed; a WebSocket upgrade requires follow=true."`
}

// processesResponse is every process of one sandbox, in the order they were first run.
type processesResponse struct {
	Processes []models.Process `json:"processes"`
}

// processKillRequest says how a kill ends the process: TERM and KILL after the grace, or KILL at once with force.
type processKillRequest struct {
	Force bool `json:"force,omitempty"`
}

// runProcess starts a named process under shard-init in a running sandbox, and answers once it started.
func (h *Handler) runProcess(ctx context.Context, in *sandboxRequest[sandbox.RunRequest]) (*reply[models.Process], error) {
	return answer(h.lifecycle.Run(ctx, in.ID, in.Body))
}

func (h *Handler) listProcesses(ctx context.Context, in *sandboxPath) (*reply[processesResponse], error) {
	procs, err := h.lifecycle.Processes(ctx, in.ID)

	return answer(processesResponse{Processes: listOf(procs)}, err)
}

func (h *Handler) getProcess(ctx context.Context, in *processPath) (*reply[models.Process], error) {
	return answer(h.lifecycle.Process(ctx, in.ID, in.Name))
}

// killProcess ends the process and cancels its starts again; the sandbox stays running.
func (h *Handler) killProcess(ctx context.Context, in *processBody[processKillRequest]) (*reply[models.Process], error) {
	return answer(h.lifecycle.Kill(ctx, in.ID, in.Name, value(in.Body).Force))
}

// attachProcess answers how a process's policy ended it: a WebSocket upgrade streams its output first, and a plain request answers the end alone.
func (h *Handler) attachProcess(w http.ResponseWriter, r *http.Request) {
	if isHandshake(r) {
		h.streamProcess(w, r)

		return
	}

	p, err := h.lifecycle.AttachProcess(r.Context(), r.PathValue("id"), r.PathValue("name"), func() (io.Writer, error) { return io.Discard, nil })
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	h.writeJSON(w, http.StatusOK, p)
}

// streamProcess sends the output of the current run on stream 1, then the ended process on stream 3; every refusal comes before the 101.
func (h *Handler) streamProcess(w http.ResponseWriter, r *http.Request) {
	label := "process " + r.PathValue("name") + " of sandbox " + r.PathValue("id")

	// The poll runs under ctx, so a client that hangs up ends it even while the process writes nothing.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	answered := false
	var f *follower
	p, err := h.lifecycle.AttachProcess(ctx, r.PathValue("id"), r.PathValue("name"), func() (io.Writer, error) {
		answered = true
		opened, err := h.follow(w, r.WithContext(ctx), label)
		if err != nil {
			return nil, err
		}
		f = opened
		context.AfterFunc(f.ctx, cancel)

		return writerFunc(func(b []byte) (int, error) {
			if err := Send(f.ctx, f.conn, StreamStdout, b); err != nil {
				return 0, err
			}

			return len(b), nil
		}), nil
	})

	// Nothing was said on the wire yet, so the refusal is a status and a JSON body like every other route.
	if !answered {
		h.writeError(w, r, err)

		return
	}
	// The library answered the handshake with its own refusal, so the daemon's log is the one place left.
	if f == nil {
		h.log.Printf("api: %s: %v", label, err)

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

	f.send(StreamExit, p)
}

// describeAttachProcess names the two answers of attachProcess: the ended process, or the output and then the ended process over a WebSocket.
func describeAttachProcess(registry huma.Registry, op *huma.Operation) {
	op.Responses["200"] = response("The process once its restart policy ended it.", "application/json", schemaOf[models.Process](registry))
	op.Responses["101"] = upgrade("A WebSocket attach. Each binary message leads with its stream byte: 1 the output from the start of the current run, 3 the ended Process, 5 a FailureMessage.", map[string]*huma.Schema{
		"3": schemaOf[models.Process](registry),
		"5": schemaOf[FailureMessage](registry),
	})
}
