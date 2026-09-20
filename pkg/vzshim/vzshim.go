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

// place installs one embedded binary by rename, signed with entitlements when there are any, and skips a copy the stamp says is this build's.
func place(dir, name string, body, entitlements []byte) (string, error) {
	path := filepath.Join(dir, name)
	// codesign rewrites the bytes, so a stamp of the embedded build says whether the installed copy is this one.
	stamp := filepath.Join(dir, name+".sha256")
	sum := fmt.Appendf(nil, "%x\n", sha256.Sum256(body))
	if current, err := os.ReadFile(stamp); err == nil && bytes.Equal(current, sum) {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	tmp, err := writeTemp(dir, name, body, 0o700)
	if err != nil {
		return "", err
	}
	if entitlements != nil {
		plist, err := writeTemp(dir, entitlementsName, entitlements, 0o600)
		if err != nil {
			return "", errors.Join(err, os.Remove(tmp))
		}
		defer os.Remove(plist)
		if err := sign(tmp, plist); err != nil {
			return "", errors.Join(err, os.Remove(tmp))
		}
	}
	// A running shim keeps its old inode when the file is replaced by rename, never truncated under it.
	if err := os.Rename(tmp, path); err != nil {
		return "", errors.Join(fmt.Errorf("install %s: %w", name, err), os.Remove(tmp))
	}
	stamped, err := writeTemp(dir, stamp, sum, 0o600)
	if err != nil {
		return "", err
	}
	if err := os.Rename(stamped, stamp); err != nil {
		return "", errors.Join(fmt.Errorf("stamp %s: %w", name, err), os.Remove(stamped))
	}

	return path, nil
}

// writeTemp puts body in a file of its own under dir, so two installers never write into one path.
func writeTemp(dir, name string, body []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(dir, filepath.Base(name)+".*.tmp")
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
