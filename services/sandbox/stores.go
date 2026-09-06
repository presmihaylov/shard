package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/secret"
)

// PolicyStore is the part of egress.Store the policy verbs drive.
type PolicyStore interface {
	Set(policy models.Policy) error
	Get(name string) (models.Policy, error)
	List() ([]models.Policy, error)
	Remove(name string) error
}

// SecretStore is the part of secret.Store the secret verbs drive. No verb reads a value.
type SecretStore interface {
	Set(name, value string, destinations []string, mock string) (secret.Secret, error)
	Get(name string) (secret.Secret, error)
	List() ([]secret.Secret, error)
	Remove(name string) error
}

// ImageStore is the part of image.Service the image verbs drive.
type ImageStore interface {
	Pull(ctx context.Context, ref string) (image.Image, error)
	List() ([]image.Image, error)
	Orphaned(ref string) ([]string, error)
	Remove(ctx context.Context, ref string, free func() error) error
}

// Reapplier puts the rules the records name back on the host, for every sandbox at once.
type Reapplier interface {
	ReapplyAll(ctx context.Context) error
}

// StoresConfig is every layer the store verbs drive.
type StoresConfig struct {
	Repo     Reader
	Policies PolicyStore
	Secrets  SecretStore
	Images   ImageStore
	// Network is built on the first policy change a sandbox holds, so a daemon without one needs no root.
	Network func() (Reapplier, error)
	// PullTimeout bounds one pull; zero is no bound.
	PullTimeout time.Duration
}

// Stores owns the policy, secret and image verbs, and the sandbox records that hold what they keep.
type Stores struct {
	cfg StoresConfig
}

func NewStores(cfg StoresConfig) *Stores {
	return &Stores{cfg: cfg}
}

// HeldError is a store entry a sandbox record still names, which no removal takes out from under it.
type HeldError struct {
	// Subject names the entry, as in `policy web`, and Verb is how a record holds it, as in `held by`.
	Subject string
	Verb    string
	Users   []string
	Fix     string
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("%s is %s sandbox %s: %s", e.Subject, e.Verb, strings.Join(e.Users, ", "), e.Fix)
}

// RuleText is one --allow or --deny as the operator typed it. The daemon owns the grammar.
type RuleText struct {
	Action models.Action `json:"action"`
	Rule   string        `json:"rule"`
}

// PolicyRequest is the body of a policy PUT: the rules in the order the host evaluates them.
type PolicyRequest struct {
	Rules []RuleText `json:"rules"`
}

// SecretRequest is the body of a secret PUT. The value crosses the socket here and nowhere else.
type SecretRequest struct {
	Value        string   `json:"value"`
	Destinations []string `json:"destinations,omitempty"`
	MockValue    string   `json:"mock_value,omitempty"`
}

// SetPolicy stores the policy and puts the new rules on every sandbox that holds it at once.
func (s *Stores) SetPolicy(ctx context.Context, name string, req PolicyRequest) (models.Policy, error) {
	if err := egress.ValidName(name); err != nil {
		return models.Policy{}, &RequestError{Err: err}
	}

	policy := models.Policy{Name: name}
	for _, text := range req.Rules {
		rule, err := egress.ParseRule(text.Action, text.Rule)
		if err != nil {
			return models.Policy{}, &RequestError{Err: err}
		}
		policy.Rules = append(policy.Rules, rule)
	}

	if err := egress.Validate(policy); err != nil {
		return models.Policy{}, &RequestError{Err: err}
	}

	if err := s.cfg.Policies.Set(policy); err != nil {
		return models.Policy{}, err
	}

	users, err := s.policyUsers(name)
	if err != nil {
		return models.Policy{}, err
	}
	if len(users) == 0 {
		return policy, nil
	}

	if err := s.reapplyAll(ctx); err != nil {
		return models.Policy{}, fmt.Errorf("policy %s is stored, but the host still enforces the rules it had: %w", name, err)
	}

	return policy, nil
}

func (s *Stores) Policy(name string) (models.Policy, error) {
	return s.cfg.Policies.Get(name)
}

func (s *Stores) Policies() ([]models.Policy, error) {
	return s.cfg.Policies.List()
}

// RemovePolicy refuses while a record names the policy: a stopped sandbox counts, since start enforces it again.
func (s *Stores) RemovePolicy(name string) error {
	if _, err := s.cfg.Policies.Get(name); err != nil {
		return err
	}

	users, err := s.policyUsers(name)
	if err != nil {
		return err
	}
	if len(users) != 0 {
		return &HeldError{Subject: "policy " + name, Verb: "held by", Users: users, Fix: "remove the sandbox first"}
	}

	return s.cfg.Policies.Remove(name)
}

// policyUsers names the sandboxes whose record holds the policy.
func (s *Stores) policyUsers(name string) ([]string, error) {
	sandboxes, unreadable := s.cfg.Repo.List()
	if unreadable != nil {
		return nil, fmt.Errorf("cannot tell which sandboxes hold the policy: %w", unreadable)
	}

	var users []string
	for _, sb := range sandboxes {
		if sb.Policy == name {
			users = append(users, sb.ID)
		}
	}

	return users, nil
}

// SetSecret stores the value, which is never logged, never echoed back and never written to a record.
func (s *Stores) SetSecret(name string, req SecretRequest) (secret.Secret, error) {
	if err := secret.ValidName(name); err != nil {
		return secret.Secret{}, &RequestError{Err: err}
	}
	if req.Value == "" {
		return secret.Secret{}, &RequestError{Err: fmt.Errorf("secret %s has no value", name)}
	}

	// A sandbox holds the placeholder it was created with, so a new one would never be matched for it.
	if req.MockValue != "" {
		if err := s.placeholderFree(name, req.MockValue); err != nil {
			return secret.Secret{}, err
		}
	}

	sec, err := s.cfg.Secrets.Set(name, req.Value, req.Destinations, req.MockValue)
	if err != nil {
		return secret.Secret{}, &RequestError{Err: err}
	}

	return sec, nil
}

func (s *Stores) Secrets() ([]secret.Secret, error) {
	return s.cfg.Secrets.List()
}

// RemoveSecret refuses while a record names the secret, unless force: the placeholder then redeems nothing.
func (s *Stores) RemoveSecret(name string, force bool) error {
	if err := secret.ValidName(name); err != nil {
		return &RequestError{Err: err}
	}

	if !force {
		if err := s.ungranted(name); err != nil {
			return err
		}
	}

	// A file that does not decode is still one to remove, so only a missing one stops here.
	if _, err := s.cfg.Secrets.Get(name); errors.Is(err, secret.ErrNotFound) {
		return err
	}

	return s.cfg.Secrets.Remove(name)
}

func (s *Stores) placeholderFree(name, mock string) error {
	existing, err := s.cfg.Secrets.Get(name)
	if errors.Is(err, secret.ErrNotFound) || (err == nil && existing.MockValue == mock) {
		return nil
	}
	if err != nil {
		return err
	}

	return s.ungranted(name)
}

// ungranted refuses when a record names the secret. A stopped sandbox counts: start hands it the placeholder again.
func (s *Stores) ungranted(name string) error {
	sandboxes, unreadable := s.cfg.Repo.List()
	// A record that does not read back may name the secret, so nothing can say it is free.
	if unreadable != nil {
		return fmt.Errorf("cannot tell which sandboxes hold the secret: %w", unreadable)
	}

	var users []string
	for _, sb := range sandboxes {
		if slices.Contains(sb.Secrets, name) {
			users = append(users, sb.ID)
		}
	}

	if len(users) == 0 {
		return nil
	}

	return &HeldError{Subject: "secret " + name, Verb: "granted to", Users: users, Fix: "remove the sandbox first, or pass --force"}
}

// PullImage fetches the reference and unpacks its rootfs. A second pull of the same one needs no network.
func (s *Stores) PullImage(ctx context.Context, ref string) (image.Image, error) {
	if ref == "" {
		return image.Image{}, &RequestError{Err: errors.New("pull takes one image reference")}
	}

	if s.cfg.PullTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.PullTimeout)
		defer cancel()
	}

	return s.cfg.Images.Pull(ctx, ref)
}

func (s *Stores) Images() ([]image.Image, error) {
	return s.cfg.Images.List()
}

// RemoveImage frees the image and its rootfs. Warnings carry the blobs the removal could not reclaim.
func (s *Stores) RemoveImage(ctx context.Context, ref string, force bool) ([]string, error) {
	if ref == "" {
		return nil, &RequestError{Err: errors.New("image rm takes one image reference")}
	}

	free := func() error { return s.unreferenced(ref) }
	if force {
		free = func() error { return nil }
	}

	return s.removeImage(ctx, ref, free)
}

// PruneImages removes every image no sandbox references, a stopped sandbox being a reference too.
func (s *Stores) PruneImages(ctx context.Context) ([]string, []string, error) {
	images, err := s.cfg.Images.List()
	if err != nil {
		return nil, nil, err
	}

	var removed, warnings []string
	for _, img := range images {
		// An index entry with no reference is nothing a record could name and nothing Remove could parse.
		if img.Reference == "" {
			warnings = append(warnings, fmt.Sprintf("image %s has no reference in the index and was left alone", img.Digest))

			continue
		}

		ref := img.Reference
		notReclaimed, err := s.removeImage(ctx, ref, func() error { return s.unreferenced(ref) })
		var held *HeldError
		if errors.As(err, &held) {
			continue
		}
		if err != nil {
			return removed, warnings, err
		}

		removed = append(removed, ref)
		warnings = append(warnings, notReclaimed...)
	}

	return removed, warnings, nil
}

// removeImage turns a removal that finished without freeing its blobs into a warning: that costs disk, not correctness.
func (s *Stores) removeImage(ctx context.Context, ref string, free func() error) ([]string, error) {
	err := s.cfg.Images.Remove(ctx, ref, free)
	if errors.Is(err, image.ErrNotReclaimed) {
		return []string{err.Error()}, nil
	}
	if err != nil {
		return nil, err
	}

	return nil, nil
}

// unreferenced refuses when a record names the image, or one whose rootfs would go with it.
func (s *Stores) unreferenced(ref string) error {
	held, err := s.heldImages()
	if err != nil {
		return err
	}

	canonical, err := image.Canonical(ref)
	if err != nil {
		return &RequestError{Err: err}
	}

	users := held[canonical]

	orphaned, err := s.cfg.Images.Orphaned(ref)
	if err != nil {
		return err
	}

	images, err := s.cfg.Images.List()
	if err != nil {
		return err
	}

	for _, img := range images {
		if img.Reference != canonical && slices.Contains(orphaned, img.Digest) {
			users = append(users, held[img.Reference]...)
		}
	}

	if len(users) == 0 {
		return nil
	}

	return &HeldError{Subject: "image " + ref, Verb: "referenced by", Users: users, Fix: "remove the sandbox first, or pass --force"}
}

// heldImages maps each image reference to the sandboxes whose records name it.
func (s *Stores) heldImages() (map[string][]string, error) {
	sandboxes, unreadable := s.cfg.Repo.List()
	// A record that does not read back may name the image, so nothing can say it is free.
	if unreadable != nil {
		return nil, fmt.Errorf("cannot tell which images the sandboxes reference: %w", unreadable)
	}

	held := map[string][]string{}
	for _, sb := range sandboxes {
		held[sb.Image] = append(held[sb.Image], sb.ID)
	}

	return held, nil
}

func (s *Stores) reapplyAll(ctx context.Context) error {
	net, err := s.cfg.Network()
	if err != nil {
		return err
	}

	return net.ReapplyAll(ctx)
}
