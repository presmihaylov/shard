//go:build linux

package bundle

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"

	"github.com/moby/profiles/apparmor"
	"github.com/moby/profiles/seccomp"
)

// AppArmorProfile is the name shard loads Docker's default profile under, so it never replaces the docker-default of a Docker on the same host.
const AppArmorProfile = "shard-default"

// DockerDefault is Docker's default confinement: its seccomp profile, and its AppArmor profile where the host runs AppArmor.
func DockerDefault() ([]Option, error) {
	opts := []Option{WithSeccomp(seccomp.GetDefaultProfile)}

	on, err := appArmorOn()
	if err != nil {
		return nil, err
	}
	if !on {
		return opts, nil
	}
	if err := apparmor.InstallDefault(AppArmorProfile); err != nil {
		return nil, fmt.Errorf("load the %s AppArmor profile: %w", AppArmorProfile, err)
	}

	return append(opts, WithAppArmor(AppArmorProfile)), nil
}

// appArmorOn is the test containerd and Docker make: the module is enabled and the parser that loads a profile is on PATH.
func appArmorOn() (bool, error) {
	if _, err := os.Stat("/sys/kernel/security/apparmor"); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("probe AppArmor: %w", err)
	}

	enabled, err := os.ReadFile("/sys/module/apparmor/parameters/enabled")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("probe AppArmor: %w", err)
	}
	if !bytes.HasPrefix(enabled, []byte("Y")) {
		return false, nil
	}

	if _, err := exec.LookPath("apparmor_parser"); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return false, nil
		}

		return false, fmt.Errorf("find apparmor_parser: %w", err)
	}

	return true, nil
}
