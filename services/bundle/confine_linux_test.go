//go:build linux

package bundle_test

import (
	"slices"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/services/bundle"
)

// runc applies the filter as written, so Docker's allow-list leaves the keyring calls on its default errno (SHARD-367).
func TestDockersProfileAllowsNoKeyringCall(t *testing.T) {
	got := buildWith(t, bundle.WithSeccomp(bundle.DockerProfile))

	if got.Linux.Seccomp == nil || got.Linux.Seccomp.DefaultAction != specs.ActErrno {
		t.Fatalf("got the filter %+v, want Docker's allow-list", got.Linux.Seccomp)
	}
	for _, rule := range got.Linux.Seccomp.Syscalls {
		if rule.Action != specs.ActAllow {
			continue
		}
		for _, call := range []string{"add_key", "keyctl", "request_key"} {
			if slices.Contains(rule.Names, call) {
				t.Errorf("Docker's profile allows %s", call)
			}
		}
	}
}
