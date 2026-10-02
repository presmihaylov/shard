//go:build linux

package bundle

import (
	"os/exec"

	"github.com/moby/profiles/apparmor"
	"github.com/moby/profiles/seccomp"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// DockerProfile is Docker's default seccomp profile, which answers the keyring calls with EPERM.
func DockerProfile(spec *specs.Spec) (*specs.LinuxSeccomp, error) {
	return seccomp.GetDefaultProfile(spec)
}

// DockerDefault is Docker's default confinement: its seccomp profile, and its AppArmor profile where the module is on.
func DockerDefault() ([]Option, error) {
	profile, err := appArmorHost{sys: "/sys", lookPath: exec.LookPath, load: apparmor.InstallDefault}.options()
	if err != nil {
		return nil, err
	}

	return append(profile, WithSeccomp(DockerProfile)), nil
}
