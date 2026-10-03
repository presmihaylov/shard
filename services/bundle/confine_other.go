//go:build !linux

package bundle

import (
	"fmt"
	"runtime"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// DockerProfile is Docker's default seccomp profile, which only a Linux kernel enforces, so every Build off Linux refuses.
func DockerProfile(*specs.Spec) (*specs.LinuxSeccomp, error) {
	return nil, fmt.Errorf("docker's default seccomp profile needs Linux, not %s", runtime.GOOS)
}

// DockerDefault off Linux is the seccomp profile alone, whose Build refuses, so a provider still constructs for the reads.
func DockerDefault() ([]Option, error) {
	return []Option{WithSeccomp(DockerProfile)}, nil
}
