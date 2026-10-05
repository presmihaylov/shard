package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandboxstate"
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
	Set(name, value string, destinations []string, placeholder string) (secret.Secret, error)
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

// Compiler is the host compile a policy passes before it is stored.
type Compiler interface {
	Compiles(ctx context.Context, policy models.Policy) error
}

// StoresConfig is every layer the store verbs drive.
type StoresConfig struct {
	Repo     Reader
	Policies PolicyStore
	Compiler Compiler
	Secrets  SecretStore
	Images   ImageStore
	// Snapshots hold their image as a record does, since a create from one never pulls.
	Snapshots SnapshotLister
	// Network is built on the first policy change a sandbox holds, so a daemon without one needs no root.
	Network func() (Reapplier, error)
	// PullTimeout bounds one pull; zero is no bound.
	PullTimeout time.Duration
}

// SnapshotLister is the part of sandboxstate.Snapshots the image verbs read.
type SnapshotLister interface {
	List() ([]models.Snapshot, error)
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
	// Noun is what the users are: sandbox or snapshot.
	Noun  string
	Users []string
	Fix   string
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("%s is %s %s %s: %s", e.Subject, e.Verb, e.Noun, strings.Join(e.Users, ", "), e.Fix)
}

func (e *HeldError) Public() string { return e.Error() }

// RuleText is one --allow or --deny as the operator typed it. The daemon owns the grammar.
type RuleText struct {
	Action models.Action `json:"action" enum:"allow,deny"`
	Rule   string        `json:"rule"`
}

// PolicyRequest is the body of a policy PUT: the rules in the order the host evaluates them.
type PolicyRequest struct {
	Rules []RuleText `json:"rules,omitempty"`
}

// SecretRequest is the body of a secret PUT. The value crosses the socket here and nowhere else.
type SecretRequest struct {
	Value        string   `json:"value" minLength:"1"`
	Destinations []string `json:"destinations,omitempty" doc:"The hosts the secret goes to. The first put of a name needs one; a rotation with none keeps the old ones."`
	// Placeholder overrides the default; empty on a rotation keeps the one the secret already has.
	Placeholder string `json:"placeholder,omitempty"`
}

// SetPolicy stores the policy and puts the new rules on every sandbox that holds it at once.
func (s *Stores) SetPolicy(ctx context.Context, name string, req PolicyRequest) (PolicyView, error) {
	if err := egress.ValidName(name); err != nil {
		return PolicyView{}, &RequestError{Err: err}
	}

	policy := models.Policy{Name: name}
	for _, text := range req.Rules {
		rule, err := egress.ParseRule(text.Action, text.Rule)
		if err != nil {
			return PolicyView{}, &RequestError{Err: err}
		}
		policy.Rules = append(policy.Rules, rule)
	}

	if err := egress.Validate(policy); err != nil {
		return PolicyView{}, &RequestError{Err: err}
	}

	// A stored name that does not resolve fails every apply while a sandbox holds it, other sandboxes' creates included.
	if err := s.cfg.Compiler.Compiles(ctx, policy); err != nil {
		return PolicyView{}, &RequestError{Err: fmt.Errorf("policy %s: %w", name, err)}
	}

	if err := s.cfg.Policies.Set(policy); err != nil {
		return PolicyView{}, err
	}

	users, err := PolicyHolders(s.cfg.Repo, name)
	if err != nil {
		return PolicyView{}, &CauseError{Text: fmt.Sprintf("policy %s is stored, but the host still enforces the rules it had", name), Err: err}
	}
	if len(users) == 0 {
		return PolicyView{Policy: policy, DNS: dnsState(policy)}, nil
	}

	if err := s.reapplyAll(ctx); err != nil {
		return PolicyView{}, &CauseError{Text: fmt.Sprintf("policy %s is stored, but the host still enforces the rules it had", name), Err: err}
	}

	return PolicyView{Policy: policy, DNS: dnsState(policy)}, nil
}

// PolicyView is a stored policy and the sandboxes whose record names it. The holders are read from the
// records on every ask, so nothing stores them and nothing can go stale.
type PolicyView struct {
	models.Policy
	Holders []string `json:"holders,omitempty"`
	// DNS is computed from the rules, because nothing stores whether a policy resolves.
	DNS string `json:"dns" enum:"open,closed"`
}

// dnsState is the word the view carries, so show and create never disagree about what opens DNS.
func dnsState(policy models.Policy) string {
	if egress.Resolves(policy) {
		return "open"
	}

	return "closed"
}

func (s *Stores) Policy(name string) (PolicyView, error) {
	if err := egress.ValidName(name); err != nil {
		return PolicyView{}, &RequestError{Err: err}
	}

	policy, err := s.cfg.Policies.Get(name)
	if err != nil {
		return PolicyView{}, err
	}

	holders, err := PolicyHolders(s.cfg.Repo, name)
	// The same list rm refuses over, so show never answers 500 over a record that does not read back (SHARD-597).
	if ids := unreadableHolders(err); ids != nil {
		return PolicyView{Policy: policy, Holders: append(holders, ids...), DNS: dnsState(policy)}, nil
	}
	if err != nil {
		return PolicyView{}, err
	}

	return PolicyView{Policy: policy, Holders: holders, DNS: dnsState(policy)}, nil
}

func (s *Stores) Policies() ([]models.Policy, error) {
	return s.cfg.Policies.List()
}

// RemovePolicy refuses while a record names the policy: a stopped sandbox counts, since start enforces it again.
func (s *Stores) RemovePolicy(name string) error {
	if err := egress.ValidName(name); err != nil {
		return &RequestError{Err: err}
	}

	if _, err := s.cfg.Policies.Get(name); err != nil {
		return err
	}

	users, err := PolicyHolders(s.cfg.Repo, name)
	if ids := unreadableHolders(err); ids != nil {
		return &HeldError{Subject: "policy " + name, Verb: "possibly held by", Noun: "sandbox", Users: append(users, ids...), Fix: unreadableFix(ids)}
	}
	if err != nil {
		return err
	}
	if len(users) != 0 {
		return &HeldError{Subject: "policy " + name, Verb: "held by", Noun: "sandbox", Users: users, Fix: "remove the sandbox first"}
	}

	return s.cfg.Policies.Remove(name)
}

// unreadableFix is the way past records that do not read back, since rm cannot free a sandbox it cannot read.
func unreadableFix(ids []string) string {
	return fmt.Sprintf("fix or delete the unreadable record of %s under the daemon root first", strings.Join(ids, ", "))
}

// unreadableHolders names the records a holder scan could not read, and nil when the scan failed for any other reason (SHARD-584).
func unreadableHolders(err error) []string {
	cause, ok := errors.AsType[*CauseError](err)
	if !ok {
		return nil
	}

	return sandboxstate.UnreadableIDs(cause.Err)
}

// PolicyHolders names the sandboxes whose record holds the policy. Every ask goes through this one, so
// what show prints and what rm refuses can never disagree.
func PolicyHolders(repo Reader, name string) ([]string, error) {
	sandboxes, unreadable := repo.List()

	var holders []string
	for _, sb := range sandboxes {
		if sb.Policy == name {
			holders = append(holders, sb.ID)
		}
	}

	// A record that does not read back may name the policy, so nothing can say it is free; the readable holders still go back for a refusal to name (SHARD-584).
	if unreadable != nil {
		return holders, &CauseError{Text: "cannot tell which sandboxes hold the policy", Err: unreadable}
	}

	return holders, nil
}

// SetSecret stores the value, which is never logged, never echoed back and never written to a record.
func (s *Stores) SetSecret(name string, req SecretRequest) (secret.Secret, error) {
	if err := secret.ValidName(name); err != nil {
		return secret.Secret{}, &RequestError{Err: err}
	}
	if req.Value == "" {
		return secret.Secret{}, &RequestError{Err: fmt.Errorf("secret %s has no value", name)}
	}

	sec, err := s.cfg.Secrets.Set(name, req.Value, req.Destinations, req.Placeholder)
	var held *secret.HeldError
	if errors.As(err, &held) {
		fix := "ungrant it first, its placeholder cannot change under a guest"
		if held.Removed {
			fix = "ungrant it first, nothing records the placeholder the guest kept when the secret was removed"
		}
		return secret.Secret{}, &HeldError{Subject: "secret " + name, Verb: "granted to", Noun: "sandbox", Users: held.Holders, Fix: fix}
	}
	if _, ok := errors.AsType[*secret.InvalidError](err); ok {
		return secret.Secret{}, &RequestError{Err: err}
	}
	if err != nil {
		return secret.Secret{}, err
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

// ungranted refuses when a record names the secret. A stopped sandbox counts: start hands it the placeholder again.
func (s *Stores) ungranted(name string) error {
	users, err := SecretHolders(s.cfg.Repo, name)
	if ids := unreadableHolders(err); ids != nil {
		return &HeldError{Subject: "secret " + name, Verb: "possibly granted to", Noun: "sandbox", Users: append(users, ids...), Fix: unreadableFix(ids) + ", or pass --force"}
	}
	if err != nil {
		return err
	}

	if len(users) == 0 {
		return nil
	}

	return &HeldError{Subject: "secret " + name, Verb: "granted to", Noun: "sandbox", Users: users, Fix: "ungrant it first, remove the sandbox, or pass --force"}
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
		return nil, &RequestError{Err: errors.New("image remove takes one image reference")}
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

// imageUser is a record that holds an image, by the reference it names and the digest it resolved to.
type imageUser struct {
	id        string
	reference string
	digest    string
}

// unreferenced refuses when a record or a snapshot names the image, or one whose rootfs would go with it.
func (s *Stores) unreferenced(ref string) error {
	sandboxes, err := s.heldImages()
	if err != nil {
		return err
	}

	snapshots, err := s.snapshotImages()
	if err != nil {
		return err
	}

	canonical, err := image.Canonical(ref)
	if err != nil {
		return &RequestError{Err: err}
	}

	orphaned, err := s.cfg.Images.Orphaned(ref)
	if err != nil {
		return err
	}

	images, err := s.cfg.Images.List()
	if err != nil {
		return err
	}

	orphanedSet := map[string]bool{}
	for _, digest := range orphaned {
		orphanedSet[digest] = true
	}

	// digestByReference resolves a pending record that holds a tag, since its own digest is not set yet.
	digestByReference := map[string]string{}
	for _, img := range images {
		digestByReference[img.Reference] = img.Digest
	}

	// holders names who holds the image by reference, or one whose rootfs goes with it by digest.
	holders := func(users []imageUser) []string {
		var held []string
		for _, u := range users {
			if u.reference == canonical || orphanedSet[resolveDigest(u, digestByReference)] {
				held = append(held, u.id)
			}
		}

		return held
	}

	if users := holders(sandboxes); len(users) != 0 {
		return &HeldError{Subject: "image " + ref, Verb: "referenced by", Noun: "sandbox", Users: users, Fix: "remove the sandbox first, or pass --force"}
	}
	if users := holders(snapshots); len(users) != 0 {
		return &HeldError{Subject: "image " + ref, Verb: "referenced by", Noun: "snapshot", Users: users, Fix: "remove it first with shard snapshot remove, or pass --force"}
	}

	return nil
}

// resolveDigest is the record's own digest, else the cache digest for its reference, else the digest its by-digest reference names.
func resolveDigest(u imageUser, byReference map[string]string) string {
	if u.digest != "" {
		return u.digest
	}
	// A manifest-list reference names the list digest, but the cache keys the platform image under a child digest, so the cache wins over the literal parse (SHARD-573).
	if digest, ok := byReference[u.reference]; ok {
		return digest
	}
	if digest, ok := image.DigestOf(u.reference); ok {
		return digest
	}

	return ""
}

// heldImages lists the sandboxes whose records name an image, each with the digest it resolved to.
func (s *Stores) heldImages() ([]imageUser, error) {
	sandboxes, unreadable := s.cfg.Repo.List()
	// A record that does not read back may name the image, so nothing can say it is free.
	if unreadable != nil {
		return nil, fmt.Errorf("cannot tell which images the sandboxes reference: %w", unreadable)
	}

	users := make([]imageUser, 0, len(sandboxes))
	for _, sb := range sandboxes {
		users = append(users, imageUser{id: sb.ID, reference: sb.Image, digest: sb.Digest})
	}

	return users, nil
}

// snapshotImages lists the snapshots whose layer sits over an image, each with the digest it resolved to.
func (s *Stores) snapshotImages() ([]imageUser, error) {
	snaps, err := s.cfg.Snapshots.List()
	if err != nil {
		return nil, fmt.Errorf("cannot tell which images the snapshots reference: %w", err)
	}

	users := make([]imageUser, 0, len(snaps))
	for _, snap := range snaps {
		users = append(users, imageUser{id: snap.ID, reference: snap.Image, digest: snap.Digest})
	}

	return users, nil
}

func (s *Stores) reapplyAll(ctx context.Context) error {
	net, err := s.cfg.Network()
	if err != nil {
		return err
	}

	return net.ReapplyAll(ctx)
}
