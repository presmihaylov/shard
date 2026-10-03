// Package vzshim carries shard-vz-shim and the guest shard-init inside the daemon, apart from pkg/vz so the shim never embeds its own previous build.
package vzshim

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/presmihaylov/shard/pkg/store"
)

// The shim and the guest init are built into this directory by make, so a build without them still compiles and says so at run time.
//
//go:embed shim
var shimFS embed.FS

const (
	shimName         = "shard-vz-shim"
	initName         = "shard-init"
	entitlementsName = "entitlements.plist"
)

// ErrNoShim is what Install returns from a binary built by go build alone, without make build-darwin.
var ErrNoShim = errors.New("vzshim: this binary was built without shard-vz-shim: run make build-darwin")

// ErrNoInit is the same for the guest shard-init, the static linux binary a VM boots as PID 1.
var ErrNoInit = errors.New("vzshim: this binary was built without the guest shard-init: run make build-darwin")

// InstallInit writes the embedded guest shard-init under dir once per build, and answers its path.
func InstallInit(dir string) (string, error) {
	body, err := fs.ReadFile(shimFS, "shim/"+initName)
	if err != nil {
		return "", ErrNoInit
	}

	return place(dir, initName, body, nil)
}

// Embedded reports whether this binary carries the shim, so a test can skip instead of fail without it.
func Embedded() bool {
	_, err := fs.Stat(shimFS, "shim/"+shimName)

	return err == nil
}

// Install writes the embedded shim under dir, ad-hoc signed with the virtualization entitlement, once per build; concurrent callers each rename their own signed copy in.
func Install(dir string) (string, error) {
	body, err := fs.ReadFile(shimFS, "shim/"+shimName)
	if err != nil {
		return "", ErrNoShim
	}
	entitlements, err := fs.ReadFile(shimFS, "shim/"+entitlementsName)
	if err != nil {
		return "", fmt.Errorf("read the embedded entitlements: %w", err)
	}

	return place(dir, shimName, body, entitlements)
}

// place installs one embedded binary through store.WriteFile, signed with entitlements when there are any, and skips a copy the stamp says is this build's.
func place(dir, name string, body, entitlements []byte) (string, error) {
	path := filepath.Join(dir, name)
	stamp := filepath.Join(dir, name+".sha256")
	current, err := stamped(path, stamp, body)
	if err != nil {
		return "", err
	}
	if current {
		return path, nil
	}

	installed := body
	if entitlements != nil {
		if installed, err = signed(dir, name, body, entitlements); err != nil {
			return "", err
		}
	}
	// A running shim keeps its old inode when the file is replaced by rename, never truncated under it.
	if err := store.WriteFile(path, installed, 0o700); err != nil {
		return "", fmt.Errorf("install %s: %w", name, err)
	}
	if err := store.WriteFile(stamp, stampOf(body, installed), 0o600); err != nil {
		return "", fmt.Errorf("stamp %s: %w", name, err)
	}

	return path, nil
}

// stampOf names the embedded build and, since codesign rewrites the bytes, the file that build installed.
func stampOf(body, installed []byte) []byte {
	return fmt.Appendf(nil, "%x\n%x\n", sha256.Sum256(body), sha256.Sum256(installed))
}

// stamped hashes the installed file every time, because a crash can cut it under an intact stamp (SHARD-355).
func stamped(path, stamp string, body []byte) (bool, error) {
	want, err := os.ReadFile(stamp)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", stamp, err)
	}
	installed, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	return bytes.Equal(want, stampOf(body, installed)), nil
}

// signed answers body ad-hoc signed with entitlements; codesign works on a file, so it signs a copy of its own.
func signed(dir, name string, body, entitlements []byte) ([]byte, error) {
	tmp, err := writeTemp(dir, name, body, 0o700)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	plist, err := writeTemp(dir, entitlementsName, entitlements, 0o600)
	if err != nil {
		return nil, err
	}
	defer os.Remove(plist)
	if err := sign(tmp, plist); err != nil {
		return nil, err
	}

	out, err := os.ReadFile(tmp)
	if err != nil {
		return nil, fmt.Errorf("read the signed %s: %w", name, err)
	}

	return out, nil
}

// writeTemp puts body in a file of its own under dir, so two installers never write into one path.
func writeTemp(dir, name string, body []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create %s: %w", name, err)
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return "", errors.Join(fmt.Errorf("write %s: %w", name, err), os.Remove(f.Name()))
	}

	return f.Name(), nil
}

// An ad-hoc signature is enough to carry the entitlement on this Mac; docs/macos-signing.md says what it cannot do.
func sign(path, plist string) error {
	cmd := exec.Command("codesign", "--sign", "-", "--force", "--entitlements", plist, path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("codesign the shim: %w: %s", err, bytes.TrimSpace(out))
	}

	return nil
}
