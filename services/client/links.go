package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/presmihaylov/shard/models"
)

type linksResult struct {
	Links []models.Link `json:"links"`
}

// AddLink links the sandbox to target, or answers the link it already holds.
func (c *Client) AddLink(ctx context.Context, ref, target string) (models.Link, error) {
	var out models.Link
	if err := c.call(ctx, http.MethodPut, linkPath(ref, target), nil, &out, c.Timeout); err != nil {
		return models.Link{}, missing(ref, err)
	}

	return out, nil
}

// RemoveLink drops the link to target; a not_found names the sandbox only when the sandbox is what is missing.
func (c *Client) RemoveLink(ctx context.Context, ref, target string) error {
	if err := c.call(ctx, http.MethodDelete, linkPath(ref, target), nil, nil, c.Timeout); err != nil {
		return c.missingOrGone(ctx, ref, err)
	}

	return nil
}

// ListLinks answers the links of one sandbox, in the order they were added.
func (c *Client) ListLinks(ctx context.Context, ref string) ([]models.Link, error) {
	var out linksResult
	if err := c.call(ctx, http.MethodGet, "/v0/sandboxes/"+url.PathEscape(ref)+"/links", nil, &out, c.Timeout); err != nil {
		return nil, missing(ref, err)
	}

	return out.Links, nil
}

func linkPath(ref, target string) string {
	return "/v0/sandboxes/" + url.PathEscape(ref) + "/links/" + url.PathEscape(target)
}
