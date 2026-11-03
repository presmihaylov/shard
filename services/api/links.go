package api

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/presmihaylov/shard/models"
)

// linksResponse is every link of one sandbox, in the order they were added.
type linksResponse struct {
	Links []models.Link `json:"links"`
}

type linkPath struct {
	ID     string `path:"id" doc:"The sandbox id or name."`
	Target string `path:"target" doc:"The id or name of the sandbox the link reaches."`
}

func (h *Handler) listLinks(ctx context.Context, in *sandboxPath) (*reply[linksResponse], error) {
	links, err := h.lifecycle.ListLinks(ctx, in.ID)

	return answer(linksResponse{Links: listOf(links)}, err)
}

func (h *Handler) putLink(ctx context.Context, in *linkPath) (*reply[models.Link], error) {
	return answer(h.lifecycle.AddLink(ctx, in.ID, in.Target))
}

func (h *Handler) removeLink(ctx context.Context, in *linkPath) (*struct{}, error) {
	return done(h.lifecycle.RemoveLink(ctx, in.ID, in.Target))
}

func describePutLink(_ huma.Registry, op *huma.Operation) {
	op.Description = "Every sandbox that links the target sees it at the same address, from 10.86.0.0/16, until no link names it. A PUT of a link the sandbox already holds changes nothing. The link outlives a stop of either sandbox, and goes when the target is removed. A target that does not exist, or the sandbox itself, answers 400 invalid_request."
}
