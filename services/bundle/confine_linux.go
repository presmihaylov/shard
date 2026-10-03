//go:build linux

package bundle

import (
	"github.com/moby/profiles/seccomp"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// DockerProfile is Docker's default seccomp profile, which answers the keyring calls with EPERM.
func DockerProfile(spec *specs.Spec) (*specs.LinuxSeccomp, error) {
	return seccomp.GetDefaultProfile(spec)
}
