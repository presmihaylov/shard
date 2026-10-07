package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

func processesPath(ref string) string {
	return "/v0/sandboxes/" + url.PathEscape(ref) + "/processes"
}

func processPath(ref, name string) string {
	return processesPath(ref) + "/" + url.PathEscape(name)
}

// Run starts a named process under shard-init in a running sandbox, and answers its entry once it started.
func (c *Client) Run(ctx context.Context, ref string, req sandbox.RunRequest) (models.Process, error) {
	var out models.Process
	if err := c.call(ctx, http.MethodPost, processesPath(ref), req, &out, c.Timeout); err != nil {
		return models.Process{}, missing(ref, err)
	}

	return out, nil
}

type processesResult struct {
	Processes []models.Process `json:"processes"`
}

// Processes lists every process of a sandbox, in the order they were first run.
func (c *Client) Processes(ctx context.Context, ref string) ([]models.Process, error) {
	var out processesResult
	if err := c.call(ctx, http.MethodGet, processesPath(ref), nil, &out, c.Timeout); err != nil {
		return nil, missing(ref, err)
	}

	return out.Processes, nil
}

// Process answers one process of a sandbox as the daemon last read it.
func (c *Client) Process(ctx context.Context, ref, name string) (models.Process, error) {
	var out models.Process
	if err := c.call(ctx, http.MethodGet, processPath(ref, name), nil, &out, c.Timeout); err != nil {
		return models.Process{}, missing(ref, err)
	}

	return out, nil
}

// Kill ends a process and cancels its restarts, TERM first or KILL with force, and answers it once reaped; the sandbox stays running.
func (c *Client) Kill(ctx context.Context, ref, name string, force bool) (models.Process, error) {
	req := struct {
		Force bool `json:"force,omitempty"`
	}{Force: force}

	var out models.Process
	if err := c.call(ctx, http.MethodPost, processPath(ref, name)+"/kill", req, &out, c.plus(models.StopGrace)); err != nil {
		return models.Process{}, missing(ref, err)
	}

	return out, nil
}

// AttachProcess writes what a process writes from the start of its current run to w until its restart policy ends it, and answers it then.
func (c *Client) AttachProcess(ctx context.Context, ref, name string, w io.Writer) (p models.Process, err error) {
	what := "process " + name + " of sandbox " + ref
	conn, err := c.open(ctx, processPath(ref, name)+"/attach", what)
	if err != nil {
		return models.Process{}, missing(ref, err)
	}
	defer func() { err = errors.Join(err, closeStream(conn, err)) }()

	for {
		stream, payload, err := api.Receive(ctx, conn)
		if err != nil {
			return models.Process{}, dropped("attach to "+what, err)
		}

		switch stream {
		case api.StreamStdout:
			if err := write(w, payload); err != nil {
				return models.Process{}, err
			}
		case api.StreamExit:
			if err := json.Unmarshal(payload, &p); err != nil {
				return models.Process{}, fmt.Errorf("the daemon answered %q as how %s ended", payload, what)
			}

			return p, nil
		case api.StreamFailure:
			return models.Process{}, failureOf(payload, ref)
		default:
			return models.Process{}, fmt.Errorf("the daemon sent an unknown stream %d; upgrade shard to match the daemon", stream)
		}
	}
}
