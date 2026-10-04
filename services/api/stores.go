package api

import (
	"context"
	"net/http"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/secret"
)

// Stores is the part of sandbox.Stores the policy, secret and image routes call.
type Stores interface {
	SetPolicy(ctx context.Context, name string, req sandbox.PolicyRequest) (sandbox.PolicyView, error)
	Policy(name string) (sandbox.PolicyView, error)
	Policies() ([]models.Policy, error)
	RemovePolicy(name string) error
	SetSecret(name string, req sandbox.SecretRequest) (secret.Secret, error)
	Secrets() ([]secret.Secret, error)
	RemoveSecret(name string, force bool) error
	PullImage(ctx context.Context, ref string) (image.Image, error)
	Images() ([]image.Image, error)
	RemoveImage(ctx context.Context, ref string, force bool) ([]string, error)
	PruneImages(ctx context.Context) ([]string, []string, error)
}

type policiesResponse struct {
	Policies []models.Policy `json:"policies"`
	Next     *string         `json:"next"`
}

type secretsResponse struct {
	Secrets []secret.Secret `json:"secrets"`
	Next    *string         `json:"next"`
	// Warnings names the secret files the daemon could not read, beside the ones it could.
	Warnings []string `json:"warnings,omitempty"`
}

type imagesResponse struct {
	Images []image.Image `json:"images"`
	Next   *string       `json:"next"`
}

type pullRequest struct {
	Ref string `json:"ref"`
}

// warningsResponse is what a removal that finished with something left over answers.
type warningsResponse struct {
	Warnings []string `json:"warnings,omitempty"`
}

type pruneResponse struct {
	Removed  []string `json:"removed"`
	Warnings []string `json:"warnings,omitempty"`
}

func (h *Handler) listPolicies(_ context.Context, in *pageInput) (*reply[policiesResponse], error) {
	q, err := paged(in.Limit, in.Cursor, egress.ValidName)
	if err != nil {
		return nil, fail(err)
	}

	policies, err := h.stores.Policies()
	if err != nil {
		return nil, fail(err)
	}

	policies, next := page(policies, q, func(p models.Policy) string { return p.Name })
	for i := range policies {
		policies[i].Rules = listOf(policies[i].Rules)
	}

	return answer(policiesResponse{Policies: policies, Next: next}, nil)
}

func (h *Handler) getPolicy(_ context.Context, in *namePath) (*reply[sandbox.PolicyView], error) {
	return policyReply(h.stores.Policy(in.Name))
}

func (h *Handler) putPolicy(ctx context.Context, in *nameBody[sandbox.PolicyRequest]) (*reply[sandbox.PolicyView], error) {
	return policyReply(h.stores.SetPolicy(ctx, in.Name, value(in.Body)))
}

// policyReply answers a policy with no rules as [], as the spec promises.
func policyReply(view sandbox.PolicyView, err error) (*reply[sandbox.PolicyView], error) {
	view.Rules = listOf(view.Rules)

	return answer(view, err)
}

func (h *Handler) removePolicy(_ context.Context, in *namePath) (*struct{}, error) {
	return done(h.stores.RemovePolicy(in.Name))
}

// listSecrets answers with names and destinations. A value never leaves the host on this route.
func (h *Handler) listSecrets(_ context.Context, in *pageInput) (*reply[secretsResponse], error) {
	q, err := paged(in.Limit, in.Cursor, secret.ValidName)
	if err != nil {
		return nil, fail(err)
	}

	secrets, unreadable := h.stores.Secrets()

	warnings, err := partial[*secret.UnreadableError](unreadable, h.warning)
	if err != nil {
		return nil, fail(err)
	}

	secrets, next := page(secrets, q, func(s secret.Secret) string { return s.Name })
	for i := range secrets {
		secrets[i].Destinations = listOf(secrets[i].Destinations)
	}

	return answer(secretsResponse{Secrets: secrets, Next: next, Warnings: warnings}, nil)
}

func (h *Handler) putSecret(_ context.Context, in *nameBody[sandbox.SecretRequest]) (*reply[secret.Secret], error) {
	sec, err := h.stores.SetSecret(in.Name, value(in.Body))
	sec.Destinations = listOf(sec.Destinations)

	return answer(sec, err)
}

type removeSecretInput struct {
	Name  string `path:"name"`
	Force bool   `query:"force" doc:"Remove the secret even when a sandbox still has a grant on it."`
}

func (h *Handler) removeSecret(_ context.Context, in *removeSecretInput) (*struct{}, error) {
	return done(h.stores.RemoveSecret(in.Name, in.Force))
}

// imageShape is the cursor check of the image list: anything the registry could not name is malformed.
func imageShape(cursor string) error {
	_, err := image.Canonical(cursor)

	return err
}

func (h *Handler) listImages(w http.ResponseWriter, r *http.Request) {
	q, err := pageOf(r, imageShape)
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	images, err := h.stores.Images()
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	images, next := page(images, q, func(img image.Image) string { return img.Reference })

	h.writeJSON(w, http.StatusOK, imagesResponse{Images: images, Next: next})
}

func (h *Handler) pullImage(w http.ResponseWriter, r *http.Request) {
	var req pullRequest
	if err := decode(w, r, &req); err != nil {
		h.writeError(w, r, err)

		return
	}

	if streamed(r) {
		streamProgress(h, w, r, http.StatusOK, "pull "+req.Ref, pullLines, func(ctx context.Context) (ProgressLine, error) {
			img, err := h.stores.PullImage(ctx, req.Ref)

			return ProgressLine{Image: &img}, err
		})

		return
	}

	img, err := h.stores.PullImage(r.Context(), req.Ref)
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	h.writeJSON(w, http.StatusOK, img)
}

func (h *Handler) removeImage(w http.ResponseWriter, r *http.Request) {
	force, err := boolQuery(r, "force")
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	warnings, err := h.stores.RemoveImage(r.Context(), r.PathValue("ref"), force)
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	h.writeJSON(w, http.StatusOK, warningsResponse{Warnings: warnings})
}

func (h *Handler) pruneImages(w http.ResponseWriter, r *http.Request) {
	removed, warnings, err := h.stores.PruneImages(r.Context())
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	h.writeJSON(w, http.StatusOK, pruneResponse{Removed: removed, Warnings: warnings})
}
