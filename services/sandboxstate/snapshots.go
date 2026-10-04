package sandboxstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

// ErrSnapshotNotFound is what a read of a snapshot shard does not hold returns. Match it with errors.Is.
var ErrSnapshotNotFound = errors.New("snapshot not found")

const (
	snapshotsDir     = "snapshots"
	snapshotNamesDir = "snapshot-names"
	snapshotFile     = "snapshot.json"
	// filesDir sits beside the record, so what a create copies back never holds the record.
	filesDir = "files"
)

// Snapshots is the snapshot record repository beside the sandbox one. A record is written once and never updated.
type Snapshots struct {
	root string
}

// NewSnapshots prepares the snapshot tree under root, beside the sandbox records.
func NewSnapshots(root string) (*Snapshots, error) {
	for _, dir := range []string{snapshotsDir, snapshotNamesDir} {
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, dirPerm); err != nil {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
	}

	if err := store.SyncDir(root); err != nil {
		return nil, err
	}

	return &Snapshots{root: root}, nil
}

func (s *Snapshots) names() names {
	return names{dir: filepath.Join(s.root, snapshotNamesDir), records: snapshotsDir, noun: "snapshot"}
}

func (s *Snapshots) dir(id string) string {
	return filepath.Join(s.root, snapshotsDir, id)
}

// Files is the directory a provider filled for this snapshot, which a create names as its Seed.
func (s *Snapshots) Files(id string) (string, error) {
	if err := ValidSnapshotID(id); err != nil {
		return "", err
	}

	return filepath.Join(s.dir(id), filesDir), nil
}

// Create claims an id, lets fill copy into its files directory, writes the record and claims the name last.
func (s *Snapshots) Create(snap models.Snapshot, fill func(files string) error) (models.Snapshot, error) {
	if snap.ID != "" {
		return models.Snapshot{}, fmt.Errorf("the snapshot carries the id %q, which the repository generates", snap.ID)
	}

	if snap.Name != "" {
		if err := ValidSnapshotName(snap.Name); err != nil {
			return models.Snapshot{}, err
		}

		// A copy can take minutes, so a taken name fails before it; the claim after it decides a race.
		taken, err := s.names().exists(snap.Name)
		if err != nil {
			return models.Snapshot{}, err
		}
		if taken {
			return models.Snapshot{}, &NameTakenError{Noun: "snapshot", Name: snap.Name, Holder: s.names().holder(snap.Name)}
		}
	}

	id, err := claimIn(filepath.Join(s.root, snapshotsDir), "snapshot")
	if err != nil {
		return models.Snapshot{}, err
	}

	files := filepath.Join(s.dir(id), filesDir)
	if err := os.Mkdir(files, dirPerm); err != nil {
		return models.Snapshot{}, errors.Join(fmt.Errorf("create %s: %w", files, err), s.abandon(id))
	}

	if err := fill(files); err != nil {
		return models.Snapshot{}, errors.Join(err, s.abandon(id))
	}

	size, err := allocated(files)
	if err != nil {
		return models.Snapshot{}, errors.Join(err, s.abandon(id))
	}

	snap.ID = id
	snap.Size = size
	if err := s.write(snap); err != nil {
		return models.Snapshot{}, errors.Join(err, s.abandon(id))
	}

	if err := s.names().claim(snap.Name, id, ValidSnapshotName); err != nil {
		return models.Snapshot{}, errors.Join(err, s.abandon(id))
	}

	return snap, nil
}

// abandon gives a claimed id back: no verb reaches a directory that holds no record.
func (s *Snapshots) abandon(id string) error {
	if err := os.RemoveAll(s.dir(id)); err != nil {
		return fmt.Errorf("remove %s: %w", s.dir(id), err)
	}

	return store.SyncDir(filepath.Join(s.root, snapshotsDir))
}

func (s *Snapshots) write(snap models.Snapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the record of snapshot %s: %w", snap.ID, err)
	}

	path := filepath.Join(s.dir(snap.ID), snapshotFile)
	if err := store.WriteFile(path, append(data, '\n'), filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// Resolve turns a snapshot name into its id; anything else is already an id, and Get answers for it.
func (s *Snapshots) Resolve(ref string) (string, error) {
	return s.names().resolve(ref)
}

// Get returns the record, or ErrSnapshotNotFound.
func (s *Snapshots) Get(id string) (models.Snapshot, error) {
	if err := ValidSnapshotID(id); err != nil {
		return models.Snapshot{}, err
	}

	path := filepath.Join(s.dir(id), snapshotFile)

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return models.Snapshot{}, &models.NotFoundError{Err: fmt.Errorf("snapshot %s: %w", id, ErrSnapshotNotFound)}
	}
	if err != nil {
		return models.Snapshot{}, fmt.Errorf("read %s: %w", path, err)
	}

	var snap models.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return models.Snapshot{}, fmt.Errorf("decode %s: %w", path, err)
	}

	return snap, nil
}

// List returns every record it can read, ordered by id, and an error naming the ones it could not.
func (s *Snapshots) List() ([]models.Snapshot, error) {
	dir := filepath.Join(s.root, snapshotsDir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	snapshots := make([]models.Snapshot, 0, len(entries))

	var unreadable error

	for _, entry := range entries {
		if !entry.IsDir() || ValidSnapshotID(entry.Name()) != nil {
			continue
		}

		snap, err := s.Get(entry.Name())
		// A directory with no record is a create still copying, or one a crash cut.
		if errors.Is(err, ErrSnapshotNotFound) {
			continue
		}
		if err != nil {
			unreadable = errors.Join(unreadable, &UnreadableError{ID: entry.Name(), Err: err})

			continue
		}

		snapshots = append(snapshots, snap)
	}

	slices.SortFunc(snapshots, func(a, b models.Snapshot) int { return strings.Compare(a.ID, b.ID) })

	return snapshots, unreadable
}

// Delete removes the name, the record and the files.
func (s *Snapshots) Delete(id string) error {
	snap, err := s.Get(id)
	if err != nil {
		return err
	}

	// The name goes first: a link that outlived its snapshot would answer for an id nothing holds.
	if err := s.names().drop(snap.Name, id); err != nil {
		return err
	}

	if err := os.RemoveAll(s.dir(id)); err != nil {
		return fmt.Errorf("remove %s: %w", s.dir(id), err)
	}

	for _, dir := range []string{snapshotNamesDir, snapshotsDir} {
		if err := store.SyncDir(filepath.Join(s.root, dir)); err != nil {
			return err
		}
	}

	return nil
}

// Sweep removes, once at daemon start, each snapshot directory a cut create left with no record.
func (s *Snapshots) Sweep(report func(string)) error {
	dir := filepath.Join(s.root, snapshotsDir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}

	swept := 0
	for _, entry := range entries {
		if !entry.IsDir() || ValidSnapshotID(entry.Name()) != nil {
			continue
		}

		_, err := os.Lstat(filepath.Join(dir, entry.Name(), snapshotFile))
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read the record of snapshot %s: %w", entry.Name(), err)
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
	report(fmt.Sprintf("swept %d snapshot directories a cut create left under %s", swept, dir))

	return nil
}

// ValidSnapshotName refuses a snapshot name no verb could take back, as ValidName does for a sandbox.
func ValidSnapshotName(name string) error { return validName("snapshot", name) }

// ValidSnapshotID refuses an id that is not one plain path component.
func ValidSnapshotID(id string) error { return plainComponent("snapshot", "id", id) }

// allocated is what the tree holds on the host, by blocks: a sparse disk counts what it filled, and a hard link counts once.
func allocated(dir string) (int64, error) {
	var total int64
	seen := map[uint64]bool{}

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("stat %s: no block count", path)
		}
		if seen[st.Ino] {
			return nil
		}
		seen[st.Ino] = true
		total += st.Blocks * 512

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", dir, err)
	}

	return total, nil
}
