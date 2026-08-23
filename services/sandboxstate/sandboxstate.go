// Package sandboxstate persists the sandbox records under the shard root. Files only, no database.
package sandboxstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

// ErrNotFound is what a read of a sandbox shard does not hold returns. Match it with errors.Is.
var ErrNotFound = errors.New("sandbox not found")

const (
	sandboxesDir = "sandboxes"
	namesDir     = "names"
	snapshotsDir = "snapshots"
	locksDir     = "locks"
	recordFile   = "sandbox.json"

	dirPerm    = 0o750
	filePerm   = 0o640
	lockPerm   = 0o600
	maxChars   = 64
	idAttempts = 10
)

// Repository is the sandbox record repository. It locks one sandbox at a time, never the whole tree,
// so no method ever holds two locks and there is no lock order to get wrong.
type Repository struct {
	root string
}

// New prepares the state tree under root, which is /var/lib/shard on the box.
func New(root string) (*Repository, error) {
	for _, dir := range []string{sandboxesDir, namesDir, snapshotsDir, locksDir} {
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, dirPerm); err != nil {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
	}

	if err := store.SyncDir(root); err != nil {
		return nil, err
	}

	return &Repository{root: root}, nil
}

// Dir is the StateDir a provider owns. It validates: the caller hands the path to mount and RemoveAll.
func (r *Repository) Dir(id string) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}

	return r.dir(id), nil
}

// SnapshotDir is where a pause writes and a fork reads. It is not created until one happens.
func (r *Repository) SnapshotDir(id string) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}

	return r.snapshotDir(id), nil
}

func (r *Repository) dir(id string) string {
	return filepath.Join(r.root, sandboxesDir, id)
}

func (r *Repository) snapshotDir(id string) string {
	return filepath.Join(r.root, snapshotsDir, id)
}

// Create generates the id, claims it and writes the record. It returns the sandbox that it stored.
// It takes no lock: the mkdir that claims the id is atomic, and the record write is atomic too.
func (r *Repository) Create(sb models.Sandbox) (models.Sandbox, error) {
	if sb.ID != "" {
		return models.Sandbox{}, fmt.Errorf("the sandbox carries the id %q, which the repository generates", sb.ID)
	}

	if !sb.State.Valid() {
		return models.Sandbox{}, fmt.Errorf("the new sandbox has an unknown state %q", sb.State)
	}

	id, err := r.claimID()
	if err != nil {
		return models.Sandbox{}, err
	}

	sb.ID = id
	if err := r.write(sb); err != nil {
		// Give the id back: no verb can reach a claimed directory that holds no record.
		return models.Sandbox{}, errors.Join(err, os.RemoveAll(r.dir(id)))
	}

	// The name is claimed last, so a crash costs this sandbox its name and never leaks the name to
	// a record no verb can reach.
	if err := r.claimName(sb.Name, id); err != nil {
		return models.Sandbox{}, errors.Join(err, os.RemoveAll(r.dir(id)))
	}

	return sb, nil
}

// claimName makes the kernel decide uniqueness a second time: symlink refuses the second claim of
// the same name, so two creates racing for one name never both win.
func (r *Repository) claimName(name, id string) error {
	if name == "" {
		return nil
	}

	if err := ValidName(name); err != nil {
		return err
	}

	err := os.Symlink(filepath.Join("..", sandboxesDir, id), r.namePath(name))
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("the name %q is taken by sandbox %s", name, r.nameHolder(name))
	}
	if err != nil {
		return fmt.Errorf("claim the name %q: %w", name, err)
	}

	return store.SyncDir(filepath.Join(r.root, namesDir))
}

// nameHolder is for the collision error only, so an unreadable link answers with a placeholder
// rather than turning one clear refusal into two errors an operator has to read.
func (r *Repository) nameHolder(name string) string {
	id, err := os.Readlink(r.namePath(name))
	if err != nil {
		return "another sandbox"
	}

	return filepath.Base(id)
}

// dropName unlinks the name only while it still points at this id. A create that took the name back
// after a half-done delete holds it now, and this sandbox has no claim on it any more.
func (r *Repository) dropName(name, id string) error {
	if name == "" {
		return nil
	}

	path := r.namePath(name)

	holder, err := os.Readlink(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the name link %s: %w", path, err)
	}
	if filepath.Base(holder) != id {
		return nil
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}

func (r *Repository) namePath(name string) string {
	return filepath.Join(r.root, namesDir, name)
}

// Resolve turns what an operator typed into the id every other method takes. A name is a symlink, so
// this is one readlink; anything else is already an id, and Get answers for one that names nothing.
func (r *Repository) Resolve(ref string) (string, error) {
	if err := validReference(ref); err != nil {
		return "", err
	}

	// ENOENT is no such name and EINVAL is an entry that is not a link; every other error is real.
	target, err := os.Readlink(r.namePath(ref))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.EINVAL) {
		return ref, nil
	}
	if err != nil {
		return "", fmt.Errorf("read the name link %s: %w", r.namePath(ref), err)
	}

	id := filepath.Base(target)
	if err := validID(id); err != nil {
		return "", fmt.Errorf("the name %q points at something that is not a sandbox id: %w", ref, err)
	}

	return id, nil
}

// claimID makes the kernel decide uniqueness: mkdir refuses the second claim of the same id.
func (r *Repository) claimID() (string, error) {
	for range idAttempts {
		id, err := newID()
		if err != nil {
			return "", err
		}

		dir := r.dir(id)

		err = os.Mkdir(dir, dirPerm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create %s: %w", dir, err)
		}

		// The record's own write syncs dir; this makes dir itself survive a power loss too.
		if err := store.SyncDir(filepath.Join(r.root, sandboxesDir)); err != nil {
			return "", err
		}

		return id, nil
	}

	return "", fmt.Errorf("no free sandbox id after %d attempts", idAttempts)
}

// Update applies mutate to the record and writes the result back. The lock spans the read and the
// write, so two callers never lose each other's field. mutate holds the lock: it must not call the
// repository again, and it must not do slow work.
func (r *Repository) Update(id string, mutate func(*models.Sandbox) error) (err error) {
	l, err := r.writeLock(id)
	if err != nil {
		return err
	}
	defer unlock(l, &err)

	sb, err := r.Get(id)
	if err != nil {
		return err
	}

	name := sb.Name

	if err := mutate(&sb); err != nil {
		return err
	}

	if sb.ID != id {
		return fmt.Errorf("the update of sandbox %s changed its id to %s", id, sb.ID)
	}

	// The name is claimed by a symlink, so a rename here would leave the index answering for the old one.
	if sb.Name != name {
		return fmt.Errorf("the update of sandbox %s changed its name to %q, which the repository does not rename", id, sb.Name)
	}

	if !sb.State.Valid() {
		return fmt.Errorf("the update of sandbox %s set an unknown state %q", id, sb.State)
	}

	return r.write(sb)
}

// Delete removes the record, the provider's directory and any snapshot the sandbox left behind.
func (r *Repository) Delete(id string) (err error) {
	l, err := r.writeLock(id)
	if err != nil {
		return err
	}
	defer unlock(l, &err)

	sb, err := r.Get(id)
	if err != nil {
		return err
	}

	// The name goes first: a link that outlived its sandbox would answer for an id nothing holds.
	if err := r.dropName(sb.Name, id); err != nil {
		return err
	}

	// The snapshot goes next: it is the one a half-done delete would leave with no id to reach it.
	// The lock file goes last, once the record is gone. Removing it while we hold it is fine, because
	// store.Acquire moves a waiter onto the file that has the name.
	for _, path := range []string{r.snapshotDir(id), r.dir(id), l.Path()} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	// Without this a power loss can bring the sandbox back, and claimID syncs the create side already.
	for _, dir := range []string{namesDir, snapshotsDir, sandboxesDir, locksDir} {
		if err := store.SyncDir(filepath.Join(r.root, dir)); err != nil {
			return err
		}
	}

	return nil
}

// Get returns the record, or ErrNotFound. It takes no lock, so it never blocks and never blocks a
// writer. A record arrives by rename, so a reader sees the whole old one or the whole new one.
func (r *Repository) Get(id string) (models.Sandbox, error) {
	if err := validID(id); err != nil {
		return models.Sandbox{}, err
	}

	path := filepath.Join(r.dir(id), recordFile)

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

	path := filepath.Join(r.dir(sb.ID), recordFile)
	if err := store.WriteFile(path, append(data, '\n'), filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// List returns every record it can read, ordered by id, and an error naming the ones it could not.
// Both can be set at once: one unreadable record must not hide every other sandbox from an operator,
// because the sandbox behind it still holds a process, a netns and its rules.
//
// It takes no lock, because it holds none of them open: a record arrives by rename, so each one
// reads whole, and the set is a walk rather than a snapshot.
func (r *Repository) List() ([]models.Sandbox, error) {
	dir := filepath.Join(r.root, sandboxesDir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	sandboxes := make([]models.Sandbox, 0, len(entries))

	var unreadable error

	for _, entry := range entries {
		// Anything that could not be an id is not a sandbox.
		if !entry.IsDir() || validID(entry.Name()) != nil {
			continue
		}

		sb, err := r.Get(entry.Name())
		// A directory with no record is a claimed id whose write has not landed, or a half-done delete.
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			unreadable = errors.Join(unreadable, err)

			continue
		}

		sandboxes = append(sandboxes, sb)
	}

	slices.SortFunc(sandboxes, func(a, b models.Sandbox) int { return strings.Compare(a.ID, b.ID) })

	return sandboxes, unreadable
}

// writeLock serializes the writers of one sandbox. Two sandboxes never wait on each other.
func (r *Repository) writeLock(id string) (*store.Lock, error) {
	path, err := r.lockPath(id)
	if err != nil {
		return nil, err
	}

	// Refuse before Acquire creates the file, or a mistyped id would leave a lock file behind forever.
	dir := r.dir(id)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("sandbox %s: %w", id, ErrNotFound)
		}

		return nil, fmt.Errorf("stat %s: %w", dir, err)
	}

	return store.Acquire(path, lockPerm)
}

// The lock file lives outside the directory Delete removes: inside it, a delete and a concurrent
// acquire would end up holding two different inodes and both would think they own the sandbox.
func (r *Repository) lockPath(id string) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}

	return filepath.Join(r.root, locksDir, id+".lock"), nil
}

// unlock joins the release into the caller's error: a lock we cannot drop blocks every later caller.
func unlock(l *store.Lock, err *error) {
	*err = errors.Join(*err, l.Release())
}

// generatedIDShape is what generateID makes. A name of that shape could shadow another sandbox's id, so it is
// refused at the door rather than resolved by a precedence rule nobody would remember.
var generatedIDShape = regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9a-f]{4}$`)

// ValidName refuses a name no verb could take back. It is a link name under the root, so it carries
// the same restrictions as an id, and it may not be spelled like one.
func ValidName(name string) error {
	if err := plainComponent("name", name); err != nil {
		return err
	}

	if generatedIDShape.MatchString(name) {
		return fmt.Errorf("the sandbox name %q is spelled like a generated id, which no name may be", name)
	}

	return nil
}

// validReference is validID for what an operator typed, which may be either an id or a name.
func validReference(ref string) error { return plainComponent("id or name", ref) }

// The id is a directory name under the root, so anything that is not one plain component is refused.
func validID(id string) error { return plainComponent("id", id) }

// plainComponent carries the noun, so a refused name never reads as a refused id.
func plainComponent(kind, s string) error {
	if s == "" {
		return fmt.Errorf("the sandbox %s is empty", kind)
	}

	if len(s) > maxChars {
		return fmt.Errorf("the sandbox %s %q is longer than %d characters", kind, s, maxChars)
	}

	for _, c := range s {
		alphanumeric := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alphanumeric && c != '-' && c != '_' {
			return fmt.Errorf("the sandbox %s %q holds %q, which is not a letter, a digit, - or _", kind, s, c)
		}
	}

	return nil
}
