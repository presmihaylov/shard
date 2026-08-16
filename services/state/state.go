// Package state persists the sandbox records under the shard root. Files only, no database.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

// ErrNotFound is what a read of a sandbox shard does not hold returns. Match it with errors.Is.
var ErrNotFound = errors.New("sandbox not found")

// ErrExists is what a second Create of the same id or the same name returns.
var ErrExists = errors.New("sandbox already exists")

const (
	sandboxesDir = "sandboxes"
	snapshotsDir = "snapshots"
	recordFile   = "sandbox.json"
	lockFile     = ".lock"

	dirPerm    = 0o750
	filePerm   = 0o640
	lockPerm   = 0o600
	idMaxChars = 64
)

// Repository is the sandbox record repository. Every method takes the repository lock.
type Repository struct {
	root string
}

// New prepares the state tree under root, which is /var/lib/shard on the box.
func New(root string) (*Repository, error) {
	for _, dir := range []string{sandboxesDir, snapshotsDir} {
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, dirPerm); err != nil {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
	}

	return &Repository{root: root}, nil
}

// Dir is the per-sandbox directory a provider owns. It is the StateDir of the spec.
func (r *Repository) Dir(id string) string {
	return filepath.Join(r.root, sandboxesDir, id)
}

// SnapshotDir is where a pause writes and a fork reads. It is not created until one happens.
func (r *Repository) SnapshotDir(id string) string {
	return filepath.Join(r.root, snapshotsDir, id)
}

// Create records a new sandbox. The id and the name must both be free.
func (r *Repository) Create(sb models.Sandbox) (err error) {
	if err := validate(sb); err != nil {
		return err
	}

	l, err := r.lock()
	if err != nil {
		return err
	}
	defer unlock(l, &err)

	_, err = r.read(sb.ID)
	if err == nil {
		return fmt.Errorf("sandbox %s: %w", sb.ID, ErrExists)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}

	if err := r.nameIsFreeFor(sb.Name, sb.ID); err != nil {
		return err
	}

	dir := r.Dir(sb.ID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	return r.write(sb)
}

// Get returns the record, or ErrNotFound.
func (r *Repository) Get(id string) (sb models.Sandbox, err error) {
	l, err := r.rlock()
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock(l, &err)

	return r.read(id)
}

// ByName returns the record that carries name. An unnamed sandbox is unreachable through it.
func (r *Repository) ByName(name string) (sb models.Sandbox, err error) {
	if name == "" {
		return models.Sandbox{}, errors.New("the sandbox name is empty")
	}

	l, err := r.rlock()
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock(l, &err)

	all, err := r.list()
	if err != nil {
		return models.Sandbox{}, err
	}

	for _, candidate := range all {
		if candidate.Name == name {
			return candidate, nil
		}
	}

	return models.Sandbox{}, fmt.Errorf("sandbox named %s: %w", name, ErrNotFound)
}

// List returns every record, ordered by id.
func (r *Repository) List() (sandboxes []models.Sandbox, err error) {
	l, err := r.rlock()
	if err != nil {
		return nil, err
	}
	defer unlock(l, &err)

	return r.list()
}

// Update applies mutate to the record and writes the result back. The lock spans the read and the
// write, so two callers never lose each other's field. mutate holds the lock: it must not call the
// repository again.
func (r *Repository) Update(id string, mutate func(*models.Sandbox) error) (err error) {
	l, err := r.lock()
	if err != nil {
		return err
	}
	defer unlock(l, &err)

	sb, err := r.read(id)
	if err != nil {
		return err
	}

	if err := mutate(&sb); err != nil {
		return err
	}

	if sb.ID != id {
		return fmt.Errorf("the update of sandbox %s changed its id to %s", id, sb.ID)
	}

	if err := validate(sb); err != nil {
		return err
	}

	if err := r.nameIsFreeFor(sb.Name, id); err != nil {
		return err
	}

	return r.write(sb)
}

// Delete removes the record, the provider's directory and any snapshot the sandbox left behind.
func (r *Repository) Delete(id string) (err error) {
	l, err := r.lock()
	if err != nil {
		return err
	}
	defer unlock(l, &err)

	if _, err := r.read(id); err != nil {
		return err
	}

	for _, dir := range []string{r.Dir(id), r.SnapshotDir(id)} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
	}

	return nil
}

func (r *Repository) read(id string) (models.Sandbox, error) {
	if err := validID(id); err != nil {
		return models.Sandbox{}, err
	}

	path := filepath.Join(r.Dir(id), recordFile)

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return models.Sandbox{}, fmt.Errorf("sandbox %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return models.Sandbox{}, fmt.Errorf("read %s: %w", path, err)
	}

	var sb models.Sandbox
	if err := json.Unmarshal(data, &sb); err != nil {
		return models.Sandbox{}, fmt.Errorf("decode %s: %w", path, err)
	}

	return sb, nil
}

func (r *Repository) write(sb models.Sandbox) error {
	data, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the record of sandbox %s: %w", sb.ID, err)
	}

	path := filepath.Join(r.Dir(sb.ID), recordFile)
	if err := store.WriteFile(path, append(data, '\n'), filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

func (r *Repository) list() ([]models.Sandbox, error) {
	dir := filepath.Join(r.root, sandboxesDir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	sandboxes := make([]models.Sandbox, 0, len(entries))

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		sb, err := r.read(entry.Name())
		if err != nil {
			return nil, err
		}

		sandboxes = append(sandboxes, sb)
	}

	slices.SortFunc(sandboxes, func(a, b models.Sandbox) int { return strings.Compare(a.ID, b.ID) })

	return sandboxes, nil
}

// nameIsFreeFor lets the sandbox keep the name it already holds, so an update is not a conflict.
func (r *Repository) nameIsFreeFor(name, id string) error {
	if name == "" {
		return nil
	}

	all, err := r.list()
	if err != nil {
		return err
	}

	for _, sb := range all {
		if sb.Name == name && sb.ID != id {
			return fmt.Errorf("sandbox named %s: %w", name, ErrExists)
		}
	}

	return nil
}

func (r *Repository) lock() (*store.Lock, error) {
	return store.Acquire(r.lockPath(), lockPerm)
}

func (r *Repository) rlock() (*store.Lock, error) {
	return store.AcquireShared(r.lockPath(), lockPerm)
}

func (r *Repository) lockPath() string {
	return filepath.Join(r.root, sandboxesDir, lockFile)
}

// unlock joins the release into the caller's error: a lock we cannot drop blocks every later caller.
func unlock(l *store.Lock, err *error) {
	*err = errors.Join(*err, l.Release())
}

func validate(sb models.Sandbox) error {
	if err := validID(sb.ID); err != nil {
		return err
	}

	if !sb.State.Valid() {
		return fmt.Errorf("sandbox %s has an unknown state %q", sb.ID, sb.State)
	}

	return nil
}

// The id is a directory name under the root, so anything that is not one plain component is refused.
func validID(id string) error {
	if id == "" {
		return errors.New("the sandbox id is empty")
	}

	if len(id) > idMaxChars {
		return fmt.Errorf("the sandbox id %q is longer than %d characters", id, idMaxChars)
	}

	for _, c := range id {
		alphanumeric := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alphanumeric && c != '-' && c != '_' {
			return fmt.Errorf("the sandbox id %q holds %q, which is not a letter, a digit, - or _", id, c)
		}
	}

	return nil
}
