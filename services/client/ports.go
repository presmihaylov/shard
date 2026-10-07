package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

type portsResult struct {
	Ports []models.Port `json:"ports"`
}

// AddPort forwards hostPort into the sandbox, or changes the forward it already has there.
func (c *Client) AddPort(ctx context.Context, ref string, hostPort uint16, req sandbox.PortRequest) (models.Port, error) {
	var out models.Port
	if err := c.call(ctx, http.MethodPut, portPath(ref, hostPort), req, &out, c.Timeout); err != nil {
		return models.Port{}, missing(ref, err)
	}

	return out, nil
}

// RemovePort ends the forward on hostPort; a not_found names the sandbox only when the sandbox is what is missing.
func (c *Client) RemovePort(ctx context.Context, ref string, hostPort uint16) error {
	if err := c.call(ctx, http.MethodDelete, portPath(ref, hostPort), nil, nil, c.Timeout); err != nil {
		return c.missingOrGone(ctx, ref, err)
	}

	return nil
}

// ListPorts answers the forwards of one sandbox, or of every sandbox when ref is empty, in host port order.
func (c *Client) ListPorts(ctx context.Context, ref string) ([]models.Port, error) {
	path := "/v0/ports"
	if ref != "" {
		path = "/v0/sandboxes/" + url.PathEscape(ref) + "/ports"
	}

	var out portsResult
	if err := c.call(ctx, http.MethodGet, path, nil, &out, c.Timeout); err != nil {
		return nil, missing(ref, err)
	}

	return out.Ports, nil
}

func portPath(ref string, hostPort uint16) string {
	return "/v0/sandboxes/" + url.PathEscape(ref) + "/ports/" + strconv.Itoa(int(hostPort))
}
