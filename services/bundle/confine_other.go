//go:build !linux

package bundle

import (
	"fmt"
	"runtime"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// AppArmorProfile is the name shard loads Docker's default profile under, so it never replaces the docker-default of a Docker on the same host.
const AppArmorProfile = "shard-default"

// DockerDefault is Docker's default confinement, which only a Linux kernel enforces, so every Build off Linux refuses.
func DockerDefault() ([]Option, error) {
	return []Option{WithSeccomp(func(*specs.Spec) (*specs.LinuxSeccomp, error) {
		return nil, fmt.Errorf("docker's default confinement needs Linux, not %s", runtime.GOOS)
	})}, nil
}
