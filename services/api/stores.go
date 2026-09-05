package api

import (
	"context"
	"net/http"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/secret"
)

// Stores is the part of sandbox.Stores the policy, secret and image routes call.
type Stores interface {
	SetPolicy(ctx context.Context, name string, req sandbox.PolicyRequest) (models.Policy, error)
	Policy(name string) (models.Policy, error)
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
}

type secretsResponse struct {
	Secrets []secret.Secret `json:"secrets"`
	// Warnings names the secret files the daemon could not read, beside the ones it could.
	Warnings []string `json:"warnings,omitempty"`
}

type imagesResponse struct {
	Images []image.Image `json:"images"`
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

func (h *Handler) listPolicies(w http.ResponseWriter, _ *http.Request) {
	policies, err := h.stores.Policies()
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, policiesResponse{Policies: policies})
}

func (h *Handler) getPolicy(w http.ResponseWriter, r *http.Request) {
	policy, err := h.stores.Policy(r.PathValue("name"))
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, policy)
}

func (h *Handler) putPolicy(w http.ResponseWriter, r *http.Request) {
	var req sandbox.PolicyRequest
	if err := decode(r, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	policy, err := h.stores.SetPolicy(r.Context(), r.PathValue("name"), req)
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, policy)
}

func (h *Handler) removePolicy(w http.ResponseWriter, r *http.Request) {
	if err := h.stores.RemovePolicy(r.PathValue("name")); err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// listSecrets answers with names and destinations. A value never leaves the host on this route.
func (h *Handler) listSecrets(w http.ResponseWriter, _ *http.Request) {
	secrets, unreadable := h.stores.Secrets()

	var warnings []string
	if unreadable != nil {
		warnings = []string{unreadable.Error()}
	}

	h.writeJSON(w, http.StatusOK, secretsResponse{Secrets: secrets, Warnings: warnings})
}

func (h *Handler) putSecret(w http.ResponseWriter, r *http.Request) {
	var req sandbox.SecretRequest
	if err := decode(r, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	sec, err := h.stores.SetSecret(r.PathValue("name"), req)
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, sec)
}

func (h *Handler) removeSecret(w http.ResponseWriter, r *http.Request) {
	force, err := boolQuery(r, "force")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	if err := h.stores.RemoveSecret(r.PathValue("name"), force); err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listImages(w http.ResponseWriter, _ *http.Request) {
	images, err := h.stores.Images()
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, imagesResponse{Images: images})
}

func (h *Handler) pullImage(w http.ResponseWriter, r *http.Request) {
	var req pullRequest
	if err := decode(r, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	img, err := h.stores.PullImage(r.Context(), req.Ref)
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, img)
}

func (h *Handler) removeImage(w http.ResponseWriter, r *http.Request) {
	force, err := boolQuery(r, "force")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	warnings, err := h.stores.RemoveImage(r.Context(), r.PathValue("ref"), force)
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, warningsResponse{Warnings: warnings})
}

func (h *Handler) pruneImages(w http.ResponseWriter, r *http.Request) {
	removed, warnings, err := h.stores.PruneImages(r.Context())
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, pruneResponse{Removed: removed, Warnings: warnings})
}
