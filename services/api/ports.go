package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// portsResponse is the page: the forwards in host port order, and the cursor of the next page or null.
type portsResponse struct {
	Ports []models.Port `json:"ports"`
	Next  *string       `json:"next"`
}

type listSandboxPortsInput struct {
	ID     string `path:"id" doc:"The sandbox id or name."`
	Limit  int    `query:"limit" minimum:"1" doc:"The most items on a page; absent returns the whole list."`
	Cursor string `query:"cursor" doc:"The next value of the previous page; this page starts after it."`
}

type portPath struct {
	ID       string `path:"id" doc:"The sandbox id or name."`
	HostPort uint16 `path:"host_port" minimum:"1" maximum:"65535" doc:"The port on the host the forward listens on."`
}

type putPortInput struct {
	ID       string `path:"id" doc:"The sandbox id or name."`
	HostPort uint16 `path:"host_port" minimum:"1" maximum:"65535" doc:"The port on the host the forward listens on."`
	Body     sandbox.PortRequest
}

func (h *Handler) listPorts(ctx context.Context, in *pageInput) (*reply[portsResponse], error) {
	return h.ports(ctx, "", in.Limit, in.Cursor)
}

func (h *Handler) listSandboxPorts(ctx context.Context, in *listSandboxPortsInput) (*reply[portsResponse], error) {
	return h.ports(ctx, in.ID, in.Limit, in.Cursor)
}

func (h *Handler) ports(ctx context.Context, ref string, limit int, cursor string) (*reply[portsResponse], error) {
	after, err := hostPortCursor(cursor)
	if err != nil {
		return nil, fail(err)
	}

	rows, err := h.lifecycle.ListPorts(ctx, ref)
	if err != nil {
		return nil, fail(err)
	}
	rows, next := pagePorts(rows, limit, after)

	return answer(portsResponse{Ports: rows, Next: next}, nil)
}

func (h *Handler) putPort(ctx context.Context, in *putPortInput) (*reply[models.Port], error) {
	return answer(h.lifecycle.AddPort(ctx, in.ID, in.HostPort, in.Body))
}

func (h *Handler) removePort(ctx context.Context, in *portPath) (*struct{}, error) {
	return done(h.lifecycle.RemovePort(ctx, in.ID, in.HostPort))
}

func describePutPort(_ huma.Registry, op *huma.Operation) {
	op.Description = "A PUT of the forward the host port already has changes nothing. A running sandbox listens at once; any other keeps the forward for its next start. The answer is 409 in_use when another sandbox forwards the host port or another process on the host listens on it."
}

// hostPortCursor reads a ports cursor as a number, since as text 10000 sorts before 9000.
func hostPortCursor(cursor string) (uint16, error) {
	if cursor == "" {
		return 0, nil
	}

	port, err := strconv.ParseUint(cursor, 10, 16)
	if err != nil {
		return 0, &sandbox.RequestError{Err: fmt.Errorf("the query cursor is malformed: %q is not a host port: %w", cursor, err)}
	}
	if port == 0 {
		return 0, &sandbox.RequestError{Err: errors.New("the query cursor is malformed: 0 is not a host port")}
	}

	return uint16(port), nil
}

// pagePorts keeps the forwards past the host port after, caps them at limit, and names the last one only when more remain.
func pagePorts(rows []models.Port, limit int, after uint16) ([]models.Port, *string) {
	from := sort.Search(len(rows), func(i int) bool { return rows[i].HostPort > after })
	rows = rows[from:]

	if limit == 0 || len(rows) <= limit {
		return listOf(rows), nil
	}
	last := strconv.Itoa(int(rows[limit-1].HostPort))

	return rows[:limit], &last
}
