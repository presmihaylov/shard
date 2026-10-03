package serve

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/presmihaylov/shard/pkg/store"
)

const (
	// AuthDir is the directory under the root that holds the default signing key and the ledger beside it.
	AuthDir = "auth"
	// SigningKeyFileName is the default signing key inside AuthDir.
	SigningKeyFileName = "signing-key"
	// signingKeyBytes is the random width of a generated key, the HS256 hash width; it is written hex.
	signingKeyBytes = 32
	// authDirLockWait bounds the wait for a concurrent first use to finish setting up the auth directory.
	authDirLockWait = 10 * time.Second
)

// SigningKeyPath is the named --signing-key-file, which must exist, else <root>/auth/signing-key.
func SigningKeyPath(root, explicit string) (string, error) {
	if explicit == "" {
		return filepath.Join(root, AuthDir, SigningKeyFileName), nil
	}

	_, err := os.Stat(explicit)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("--signing-key-file %s does not exist: a named key file must exist, and only the default %s is created", explicit, filepath.Join(root, AuthDir, SigningKeyFileName))
	}
	if err != nil {
		return "", fmt.Errorf("read the signing key file %s: %w", explicit, err)
	}

	return explicit, nil
}

// SigningKey loads the key at SigningKeyPath and answers its path; only the default key is created when missing.
func SigningKey(root, explicit string) ([]byte, string, error) {
	path, err := SigningKeyPath(root, explicit)
	if err != nil {
		return nil, "", err
	}

	key, err := readSigningKey(path)
	if explicit != "" || err == nil {
		return key, path, err
	}
	// A concurrent first use holds the new auth dir at the umask's mode until its chmod, which reads as a permission error.
	if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission) {
		return nil, path, err
	}

	key, err = createSigningKey(path)

	return key, path, err
}

// readSigningKey refuses a key file others can read, because the key signs and checks every token; no error holds the value.
func readSigningKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read the signing key file %s: %w", path, err)
	}
	// A fifo would block the read forever, and a directory holds no key.
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("the signing key file %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o007 != 0 {
		return nil, fmt.Errorf("the signing key file %s is at mode %04o, which everyone on the host can read", path, info.Mode().Perm())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the signing key file %s: %w", path, err)
	}

	key := strings.TrimSpace(string(raw))
	if key == "" {
		return nil, fmt.Errorf("the signing key file %s holds no key", path)
	}
	// RFC 7518 wants an HS256 key at least the hash width, 32 bytes, or an offline brute force breaks a short one.
	if len(key) < 32 {
		return nil, fmt.Errorf("the signing key in %s is %d bytes; a signing key needs at least 32: openssl rand -hex 32 > %s", path, len(key), path)
	}

	return []byte(key), nil
}

// createSigningKey writes a random key to path at 0600, or reads the one a concurrent first use landed there first.
func createSigningKey(path string) ([]byte, error) {
	dir := filepath.Dir(path)
	if err := makeAuthDir(dir); err != nil {
		return nil, err
	}
	key, err := readSigningKey(path)
	if err == nil {
		// The creator may not have synced its link yet, and a crash then would void every token signed with this key.
		if err := store.SyncDir(dir); err != nil {
			return nil, err
		}

		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	var raw [signingKeyBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("read random bytes for the signing key: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return nil, fmt.Errorf("create a temp file in %s: %w", dir, err)
	}
	if err := writeKey(tmp, hex.EncodeToString(raw[:])+"\n"); err != nil {
		return nil, errors.Join(err, os.Remove(tmp.Name()))
	}

	// Link never replaces, unlike rename: the loser of a concurrent first use gets EEXIST and reads the winner's key.
	linkErr := os.Link(tmp.Name(), path)
	if err := os.Remove(tmp.Name()); err != nil {
		return nil, errors.Join(fmt.Errorf("remove %s: %w", tmp.Name(), err), linkErr)
	}
	if linkErr != nil && !errors.Is(linkErr, fs.ErrExist) {
		return nil, fmt.Errorf("create the signing key file %s: %w", path, linkErr)
	}
	if err := store.SyncDir(dir); err != nil {
		return nil, err
	}

	return readSigningKey(path)
}

// makeAuthDir creates dir at 0700, and the root above it as the daemon does; a dir that exists keeps the mode its owner gave it.
func makeAuthDir(dir string) (err error) {
	root := filepath.Dir(dir)
	if err = store.MkdirAllDurable(root, 0o750); err != nil {
		return err
	}

	// A concurrent first use waits here until the creator's chmod, or the umask can lock it out of the new dir.
	lock, err := store.AcquireDir(root, authDirLockWait)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()

	err = os.Mkdir(dir, 0o700)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create the auth directory %s: %w", dir, err)
	}
	// The umask can trim the mode, and the owner needs all of 0700 to write the key.
	if err = os.Chmod(dir, 0o700); err != nil { // #nosec G302: a directory needs the execute bit, and 0700 lets only the owner in
		return fmt.Errorf("set the mode of the auth directory %s: %w", dir, err)
	}

	return store.SyncDir(root)
}

// writeKey fills the temp file at 0600 and makes it durable before anything links it into place.
func writeKey(f *os.File, key string) error {
	if _, err := f.WriteString(key); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", f.Name(), err), f.Close())
	}
	if err := f.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("set the mode of %s: %w", f.Name(), err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", f.Name(), err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", f.Name(), err)
	}

	return nil
}
