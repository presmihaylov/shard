package vz

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// The shim binary is built into this directory by make, so a build without it still compiles and says so at run time.
//
//go:embed shim
var shimFS embed.FS

const (
	shimName         = "shard-vz-shim"
	entitlementsName = "entitlements.plist"
)

// ErrNoShim is what InstallShim returns from a binary built by go build alone, without make build-darwin.
var ErrNoShim = errors.New("vz: this binary was built without shard-vz-shim: run make build-darwin")

// Embedded reports whether this binary carries the shim, so a test can skip instead of fail without it.
func Embedded() bool {
	_, err := fs.Stat(shimFS, "shim/"+shimName)

	return err == nil
}

// InstallShim writes the embedded shim under dir and ad-hoc signs it with the virtualization entitlement, once per build.
func InstallShim(dir string) (string, error) {
	body, err := fs.ReadFile(shimFS, "shim/"+shimName)
	if err != nil {
		return "", ErrNoShim
	}
	path := filepath.Join(dir, shimName)
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, body) {
		return path, nil
	}

	entitlements, err := fs.ReadFile(shimFS, "shim/"+entitlementsName)
	if err != nil {
		return "", fmt.Errorf("read the embedded entitlements: %w", err)
	}
	plist := filepath.Join(dir, entitlementsName)
	if err := os.WriteFile(plist, entitlements, 0o600); err != nil {
		return "", fmt.Errorf("write the entitlements: %w", err)
	}
	// A running shim keeps its old inode when the file is replaced by rename, never truncated under it.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o700); err != nil { //nolint:gosec // the shim must execute
		return "", fmt.Errorf("write the shim: %w", err)
	}
	if err := sign(tmp, plist); err != nil {
		return "", errors.Join(err, os.Remove(tmp))
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("install the shim: %w", err)
	}

	return path, nil
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
