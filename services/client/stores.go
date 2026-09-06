package client

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/secret"
)

// Image is one entry of the image store, as the daemon describes it.
type Image = image.Image

// Secret is one entry of the secret store: a name and its destinations, never a value.
type Secret = secret.Secret

// RuleText is one --allow or --deny as the operator typed it; the daemon owns the grammar.
type RuleText = sandbox.RuleText

// SecretsResult is what secret ls prints: the secrets the daemon read, beside the files it could not.
type SecretsResult struct {
	Secrets  []Secret `json:"secrets"`
	Warnings []string `json:"warnings,omitempty"`
}

// PruneResult names the images prune freed, and the blobs it left on disk.
type PruneResult struct {
	Removed  []string `json:"removed"`
	Warnings []string `json:"warnings,omitempty"`
}

type policiesResult struct {
	Policies []models.Policy `json:"policies"`
}

type imagesResult struct {
	Images []Image `json:"images"`
}

type warningsResult struct {
	Warnings []string `json:"warnings,omitempty"`
}

func (c *Client) ListPolicies(ctx context.Context) ([]models.Policy, error) {
	var out policiesResult
	if err := c.call(ctx, http.MethodGet, "/v0/policies", nil, &out, c.Timeout); err != nil {
		return nil, err
	}

	return out.Policies, nil
}

func (c *Client) GetPolicy(ctx context.Context, name string) (models.Policy, error) {
	var out models.Policy
	if err := c.call(ctx, http.MethodGet, "/v0/policies/"+url.PathEscape(name), nil, &out, c.Timeout); err != nil {
		return models.Policy{}, err
	}

	return out, nil
}

// SetPolicy sends the rules in the order the operator typed them, which is the order the host evaluates them.
func (c *Client) SetPolicy(ctx context.Context, name string, rules []RuleText) (models.Policy, error) {
	req := sandbox.PolicyRequest{Rules: rules}

	var out models.Policy
	if err := c.call(ctx, http.MethodPut, "/v0/policies/"+url.PathEscape(name), req, &out, c.Timeout); err != nil {
		return models.Policy{}, err
	}

	return out, nil
}

func (c *Client) RemovePolicy(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/v0/policies/"+url.PathEscape(name), nil, nil, c.Timeout)
}

func (c *Client) ListSecrets(ctx context.Context) (SecretsResult, error) {
	var out SecretsResult
	if err := c.call(ctx, http.MethodGet, "/v0/secrets", nil, &out, c.Timeout); err != nil {
		return SecretsResult{}, err
	}

	return out, nil
}

// SetSecret is the one call that carries a value. Nothing on either side logs it or answers with it.
func (c *Client) SetSecret(ctx context.Context, name, value string, destinations []string, mock string) (Secret, error) {
	req := sandbox.SecretRequest{Value: value, Destinations: destinations, MockValue: mock}

	var out Secret
	if err := c.call(ctx, http.MethodPut, "/v0/secrets/"+url.PathEscape(name), req, &out, c.Timeout); err != nil {
		return Secret{}, err
	}

	return out, nil
}

func (c *Client) RemoveSecret(ctx context.Context, name string, force bool) error {
	path := "/v0/secrets/" + url.PathEscape(name)
	if force {
		path += "?force=true"
	}

	return c.call(ctx, http.MethodDelete, path, nil, nil, c.Timeout)
}

func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	var out imagesResult
	if err := c.call(ctx, http.MethodGet, "/v0/images", nil, &out, c.Timeout); err != nil {
		return nil, err
	}

	return out.Images, nil
}

// PullImage has no bound of its own: a pull takes as long as the registry and the disk it writes.
func (c *Client) PullImage(ctx context.Context, ref string) (Image, error) {
	req := struct {
		Ref string `json:"ref"`
	}{Ref: ref}

	var out Image
	if err := c.call(ctx, http.MethodPost, "/v0/images/pull", req, &out, 0); err != nil {
		return Image{}, err
	}

	return out, nil
}

// RemoveImage answers with the blobs the removal could not reclaim, which cost disk and nothing else.
func (c *Client) RemoveImage(ctx context.Context, ref string, force bool) ([]string, error) {
	path := "/v0/images/" + imagePath(ref)
	if force {
		path += "?force=true"
	}

	var out warningsResult
	if err := c.call(ctx, http.MethodDelete, path, nil, &out, c.Timeout); err != nil {
		return nil, err
	}

	return out.Warnings, nil
}

// PruneImages sweeps every image no record names, so it takes no bound of its own either.
func (c *Client) PruneImages(ctx context.Context) (PruneResult, error) {
	var out PruneResult
	if err := c.call(ctx, http.MethodPost, "/v0/images/prune", nil, &out, 0); err != nil {
		return PruneResult{}, err
	}

	return out, nil
}

// imagePath escapes each segment on its own, because the slashes of a reference are path separators here.
func imagePath(ref string) string {
	parts := strings.Split(ref, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}

	return strings.Join(parts, "/")
}
