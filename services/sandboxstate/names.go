package sandboxstate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/presmihaylov/shard/pkg/store"
)

// names is one name index: a directory of symlinks, each into the records directory that holds the id.
type names struct {
	dir     string
	records string
	noun    string
}

func (n names) path(name string) string {
	return filepath.Join(n.dir, name)
}

// The symlink claim makes concurrent creates choose one name owner.
func (n names) claim(name, id string, valid func(string) error) error {
	if name == "" {
		return nil
	}

	if err := valid(name); err != nil {
		return err
	}

	err := os.Symlink(filepath.Join("..", n.records, id), n.path(name))
	if errors.Is(err, fs.ErrExist) {
		return &NameTakenError{Noun: n.noun, Name: name, Holder: n.holder(name)}
	}
	if err != nil {
		return fmt.Errorf("claim the name %q: %w", name, err)
	}

	return store.SyncDir(n.dir)
}

// An unreadable link answers with a placeholder, so a name collision stays one clear error.
func (n names) holder(name string) string {
	id, err := os.Readlink(n.path(name))
	if err != nil {
		return "another " + n.noun
	}

	return filepath.Base(id)
}

// A create can take the name back after a half-done delete, so drop unlinks only a link to this id.
func (n names) drop(name, id string) error {
	if name == "" {
		return nil
	}

	path := n.path(name)

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

// A case-insensitive filesystem lets os.Readlink follow a mixed-case link, so the exact entry decides (SHARD-374).
func (n names) exists(ref string) (bool, error) {
	entries, err := os.ReadDir(n.dir)
	if err != nil {
		return false, fmt.Errorf("read the names directory: %w", err)
	}

	for _, entry := range entries {
		if entry.Name() == ref {
			return true, nil
		}
	}

	return false, nil
}

// resolve turns a name into the id its link holds; anything else is already an id.
func (n names) resolve(ref string) (string, error) {
	if err := plainComponent(n.noun, "id or name", ref); err != nil {
		return "", err
	}

	// ENOENT is no such name and EINVAL is an entry that is not a link; every other error is real.
	target, err := os.Readlink(n.path(ref))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.EINVAL) {
		return ref, nil
	}
	if err != nil {
		return "", fmt.Errorf("read the name link %s: %w", n.path(ref), err)
	}

	// A case-insensitive filesystem lets readlink follow a legacy mixed-case link, so only an exact entry resolves (SHARD-374).
	exact, err := n.exists(ref)
	if err != nil {
		return "", err
	}
	if !exact {
		return ref, nil
	}

	// A refused target is a broken link, never the operator's mistake, so it is no ValidationError.
	id := filepath.Base(target)
	if plainComponent(n.noun, "id", id) != nil {
		return "", fmt.Errorf("the name %q points at %q, which is not a %s id", ref, target, n.noun)
	}

	return id, nil
}
