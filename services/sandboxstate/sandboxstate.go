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
	"sync"
	"sync/atomic"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

// ErrNotFound is what a read of a sandbox shard does not hold returns. Match it with errors.Is.
var ErrNotFound = errors.New("sandbox not found")

const (
	sandboxesDir = "sandboxes"
	namesDir     = "names"
	// checkpointsDir holds what a pause writes; snapshotsDir is the snapshot store's.
	checkpointsDir = "checkpoints"
	recordFile     = "sandbox.json"

	dirPerm    = 0o750
	filePerm   = 0o640
	maxChars   = 64
	idAttempts = 10
)

// Repository is the sandbox record repository. The daemon is the one process that writes it, so a
// read and the write that follows it are held together by a mutex and not by a file lock.
type Repository struct {
	root string

	mu sync.Mutex

	// gen bumps on every durable change to the set, so a reader caches by it and lists the records only when it moves (SHARD-381).
	gen atomic.Uint64
}

// New prepares the state tree under root, which is /var/lib/shard on the box.
func New(root string) (*Repository, error) {
	for _, dir := range []string{sandboxesDir, namesDir, checkpointsDir} {
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
	if err := ValidID(id); err != nil {
		return "", err
	}

	return r.dir(id), nil
}

// CheckpointDir is where a pause writes and a resume reads. It is not created until one happens.
func (r *Repository) CheckpointDir(id string) (string, error) {
	if err := ValidID(id); err != nil {
		return "", err
	}

	return r.checkpointDir(id), nil
}

func (r *Repository) dir(id string) string {
	return filepath.Join(r.root, sandboxesDir, id)
}

// LongestDir is the longest StateDir a sandbox under root gets, so a caller can check that what it puts there fits.
func LongestDir(root string) string {
	return filepath.Join(root, sandboxesDir, strings.Repeat("x", maxIDLength()))
}

func (r *Repository) checkpointDir(id string) string {
	return filepath.Join(r.root, checkpointsDir, id)
}

func (r *Repository) names() names {
	return names{dir: filepath.Join(r.root, namesDir), records: sandboxesDir, noun: "sandbox"}
}

// Create generates the id, claims it, runs each admit on its directory and writes the record. It returns the sandbox that it stored.
// It takes no lock: the mkdir that claims the id is atomic, and the record write is atomic too.
func (r *Repository) Create(sb models.Sandbox, admit ...func(dir string) error) (models.Sandbox, error) {
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

	// An admit runs before the record, so a refusal leaves nothing a verb can see.
	for _, check := range admit {
		if err := check(r.dir(id)); err != nil {
			return models.Sandbox{}, errors.Join(err, os.RemoveAll(r.dir(id)))
		}
	}

	sb.ID = id
	if err := r.write(sb); err != nil {
		// Give the id back: no verb can reach a claimed directory that holds no record.
		cleanup := os.RemoveAll(r.dir(id))
		// Bump again after cleanup: write's own bump already fired, and the counter must move whether or not removal cleared the record (SHARD-381).
		r.gen.Add(1)

		return models.Sandbox{}, errors.Join(err, cleanup)
	}

	// The name is claimed last, so a crash costs this sandbox its name and never leaks the name to
	// a record no verb can reach.
	if err := r.names().claim(sb.Name, id, ValidName); err != nil {
		cleanup := os.RemoveAll(r.dir(id))
		// Bump again after cleanup: write's own bump already fired, and the counter must move whether or not removal cleared the record (SHARD-381).
		r.gen.Add(1)

		return models.Sandbox{}, errors.Join(err, cleanup)
	}

	return sb, nil
}

// Resolve turns what an operator typed into the id every other method takes. A name is a symlink, so
// this is one readlink; anything else is already an id, and Get answers for one that names nothing.
func (r *Repository) Resolve(ref string) (string, error) {
	return r.names().resolve(ref)
}

// claimID makes the kernel decide uniqueness: mkdir refuses the second claim of the same id.
func (r *Repository) claimID() (string, error) {
	return claimIn(filepath.Join(r.root, sandboxesDir), "sandbox")
}

func claimIn(parent, noun string) (string, error) {
	for range idAttempts {
		id, err := newID()
		if err != nil {
			return "", err
		}

		dir := filepath.Join(parent, id)

		err = os.Mkdir(dir, dirPerm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create %s: %w", dir, err)
		}

		// The record's own write syncs dir; this makes dir itself survive a power loss too.
		if err := store.SyncDir(parent); err != nil {
			return "", err
		}

		return id, nil
	}

	return "", fmt.Errorf("no free %s id after %d attempts", noun, idAttempts)
}

// Update applies mutate to the record and writes the result back. The lock spans the read and the
// write, so two callers never lose each other's field. mutate holds the lock: it must not call the
// repository again, and it must not do slow work.
func (r *Repository) Update(id string, mutate func(*models.Sandbox) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

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

// Delete removes the record, the provider's directory and any checkpoint the sandbox left behind.
func (r *Repository) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	sb, err := r.Get(id)
	if err != nil {
		return err
	}

	// Past here the delete touches the disk, so bump on every exit: a remove or sync error must not leave a reader serving a gone record that still carries its secret (SHARD-381).
	defer r.gen.Add(1)

	// The name goes first: a link that outlived its sandbox would answer for an id nothing holds.
	if err := r.names().drop(sb.Name, id); err != nil {
		return err
	}

	// The checkpoint and its unfinished .tmp go next: nothing else reaches them once the record is gone (SHARD-368).
	for _, path := range []string{r.checkpointDir(id), r.checkpointDir(id) + ".tmp", r.dir(id)} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	// Without this a power loss can bring the sandbox back, and claimID syncs the create side already.
	for _, dir := range []string{namesDir, checkpointsDir, sandboxesDir} {
		if err := store.SyncDir(filepath.Join(r.root, dir)); err != nil {
			return err
		}
	}

	return nil
}

// SweepCheckpointTmp removes, once at daemon start, each checkpoint .tmp no record reaches; one a record names is left to its provider and the reconcile (SHARD-368, SHARD-428).
func (r *Repository) SweepCheckpointTmp(report func(string)) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	dir := filepath.Join(r.root, checkpointsDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the checkpoints directory %s: %w", dir, err)
	}

	swept := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".tmp")

		keep, note := r.recordedTmp(id)
		if note != "" {
			report(note)
		}
		if keep {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		swept++
	}
	if swept == 0 {
		return nil
	}

	if err := store.SyncDir(dir); err != nil {
		return err
	}
	report(fmt.Sprintf("swept %d orphan checkpoint staging directories the last daemon left under %s", swept, dir))

	return nil
}

// recordedTmp keeps a checkpoint .tmp whose record exists, or will not read and so may still name it (SHARD-368).
func (r *Repository) recordedTmp(id string) (bool, string) {
	if ValidID(id) != nil {
		return false, ""
	}

	_, err := r.Get(id)
	if err == nil {
		return true, ""
	}
	if errors.Is(err, ErrNotFound) {
		return false, ""
	}

	return true, fmt.Sprintf("kept the checkpoint staging %s.tmp, because its record will not read: %v", id, err)
}

// Get returns the record, or ErrNotFound. It takes no lock, so it never blocks and never blocks a
// writer. A record arrives by rename, so a reader sees the whole old one or the whole new one.
func (r *Repository) Get(id string) (models.Sandbox, error) {
	if err := ValidID(id); err != nil {
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
	// store.WriteFile can land the rename then fail its dir sync, so bump on every exit or a reader keeps the old record and serves a revoked secret (SHARD-381).
	defer r.gen.Add(1)

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

// Generation returns a counter that moves whenever the set may have changed on disk, so a reader rebuilds only when it moves; it may move without a real change but never misses one.
func (r *Repository) Generation() uint64 {
	return r.gen.Load()
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
		if !entry.IsDir() || ValidID(entry.Name()) != nil {
			continue
		}

		sb, err := r.Get(entry.Name())
		// A directory with no record is a claimed id whose write has not landed, or a half-done delete.
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			unreadable = errors.Join(unreadable, &UnreadableError{ID: entry.Name(), Err: err})

			continue
		}

		sandboxes = append(sandboxes, sb)
	}

	slices.SortFunc(sandboxes, func(a, b models.Sandbox) int { return strings.Compare(a.ID, b.ID) })

	return sandboxes, unreadable
}

// RecordedProvider is the substrate that made the records under root, and "" when the root holds none.
// It creates nothing and opens no image, so a daemon can ask it before it prepares the tree.
func RecordedProvider(root string) (string, error) {
	dir := filepath.Join(root, sandboxesDir)

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", dir, err)
	}

	r := &Repository{root: root}

	var unreadable error
	var provider string

	for _, entry := range entries {
		// Anything that could not be an id is not a sandbox.
		if !entry.IsDir() || ValidID(entry.Name()) != nil {
			continue
		}

		sb, err := r.Get(entry.Name())
		// A directory with no record is a claimed id whose write has not landed, or a half-done delete.
		if errors.Is(err, ErrNotFound) {
			continue
		}
		// Skip an unreadable record the way List does, so the records a good one names still select (SHARD-343).
		if err != nil {
			unreadable = errors.Join(unreadable, &UnreadableError{ID: entry.Name(), Err: err})

			continue
		}
		// Keep scanning past the first provider, so a record that sorts later and cannot be read is still reported (SHARD-343).
		if provider == "" && sb.Provider != "" {
			provider = sb.Provider
		}
	}

	return provider, unreadable
}

// Lister is the List a ListReadable caller holds, so a package with its own narrower records interface passes it.
type Lister interface {
	List() ([]models.Sandbox, error)
}

// ListReadable returns the readable records when one will not decode and reports the unreadable ones through ulog; any other list error fails closed (SHARD-343, SHARD-403).
func ListReadable(l Lister, ulog *UnreadableLog) ([]models.Sandbox, error) {
	sandboxes, err := l.List()
	if err == nil {
		return sandboxes, nil
	}

	// A join carrying any error other than an unreadable record is a real failure, so fail closed (SHARD-343).
	if !onlyUnreadable(err) {
		return nil, err
	}

	if ulog != nil {
		ulog.report(err)
	}

	return sandboxes, nil
}

// UnreadableLog reports each unreadable record once per daemon life, and again only when its error text changes, so one bad record does not flood the log (SHARD-403).
type UnreadableLog struct {
	logf func(string, ...any)
	mu   sync.Mutex
	seen map[string]string
}

// NewUnreadableLog builds a dedup over logf. Share one for a daemon's life, so one record logs once.
func NewUnreadableLog(logf func(string, ...any)) *UnreadableLog {
	return &UnreadableLog{logf: logf, seen: make(map[string]string)}
}

// report logs every unreadable record in err that is new or whose error text changed since the last log.
func (u *UnreadableLog) report(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	for _, e := range unreadableErrors(err) {
		text := e.Err.Error()
		if u.seen[e.ID] == text {
			continue
		}
		u.seen[e.ID] = text
		u.logf("record %s cannot be read, so it is skipped: %v", e.ID, e.Err)
	}
}

// unreadableErrors flattens the UnreadableErrors joined into err, in the order List built them.
func unreadableErrors(err error) []*UnreadableError {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []*UnreadableError
		for _, sub := range joined.Unwrap() {
			out = append(out, unreadableErrors(sub)...)
		}

		return out
	}

	var unreadable *UnreadableError
	if errors.As(err, &unreadable) {
		return []*UnreadableError{unreadable}
	}

	return nil
}

// onlyUnreadable reports whether err is non-nil and every error joined into it is an UnreadableError.
func onlyUnreadable(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs := joined.Unwrap()
		for _, e := range errs {
			if !onlyUnreadable(e) {
				return false
			}
		}

		return len(errs) > 0
	}

	var unreadable *UnreadableError

	return errors.As(err, &unreadable)
}

// generatedIDShape is what generateID makes. A name of that shape could shadow another sandbox's id, so it is
// refused at the door rather than resolved by a precedence rule nobody would remember.
var generatedIDShape = regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9a-f]{4}$`)

// ValidName refuses a name no verb could take back. It is a link name under the root, so it carries
// the same restrictions as an id, and it may not be spelled like one.
func ValidName(name string) error { return validName("sandbox", name) }

func validName(noun, name string) error {
	if err := plainComponent(noun, "name", name); err != nil {
		return err
	}

	if generatedIDShape.MatchString(name) {
		return fmt.Errorf("the %s name %q is spelled like a generated id, which no name may be", noun, name)
	}

	return nil
}

// ValidID refuses an id that is not one plain directory component under the root.
func ValidID(id string) error { return plainComponent("sandbox", "id", id) }

// UnreadableError is one record List could not read, so a caller can tell lost rows from a failed list.
type UnreadableError struct {
	ID  string
	Err error
}

func (e *UnreadableError) Error() string { return e.Err.Error() }

func (e *UnreadableError) Unwrap() error { return e.Err }

// ValidationError is a refused id or name: the caller's spelling, never the state of the host.
type ValidationError struct {
	Reason string
}

func (e *ValidationError) Error() string { return e.Reason }

// NameTakenError is a create whose name another sandbox or snapshot already holds: the caller's input, not a host fault.
type NameTakenError struct {
	// Noun is what holds the name: sandbox or snapshot.
	Noun   string
	Name   string
	Holder string
}

func (e *NameTakenError) Error() string {
	return fmt.Sprintf("the name %q is taken by %s %s", e.Name, e.Noun, e.Holder)
}

// plainComponent carries the noun and the kind, so a refused name never reads as a refused id.
func plainComponent(noun, kind, s string) error {
	if s == "" {
		return &ValidationError{Reason: fmt.Sprintf("the %s %s is empty", noun, kind)}
	}

	if len(s) > maxChars {
		return &ValidationError{Reason: fmt.Sprintf("the %s %s %q is longer than %d characters", noun, kind, s, maxChars)}
	}

	for _, c := range s {
		// A case-insensitive filesystem folds an upper-case letter onto another record, so ids, names and refs stay lower case (SHARD-374).
		if c >= 'A' && c <= 'Z' {
			return &ValidationError{Reason: fmt.Sprintf("the %s %s %q holds %q, and must be lower case: a case-insensitive filesystem would fold it onto another %s", noun, kind, s, c, noun)}
		}
		alphanumeric := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !alphanumeric && c != '-' && c != '_' {
			return &ValidationError{Reason: fmt.Sprintf("the %s %s %q holds %q, which is not a lower-case letter, a digit, - or _", noun, kind, s, c)}
		}
	}

	return nil
}
