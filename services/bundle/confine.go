package bundle

import (
	"fmt"
	"runtime"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// Option configures a Service.
type Option func(*Service)

// WithSeccomp puts the filter profile makes into every bundle. It takes the finished spec, because Docker's profile keys off the capabilities.
func WithSeccomp(profile func(*specs.Spec) (*specs.LinuxSeccomp, error)) Option {
	return func(s *Service) { s.seccomp = profile }
}

// keyringCalls reach the kernel keyring, whose quota is per host uid, so one guest that spends it fails every create that shares the uid (SHARD-367).
var keyringCalls = []string{"add_key", "keyctl", "request_key"}

// KeyringProfile allows every call but the keyring ones. A deny-list is all Sysbox keeps: it rewrites an allow-list to admit these three again.
func KeyringProfile(*specs.Spec) (*specs.LinuxSeccomp, error) {
	arches, err := nativeArches(runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	// ENOSYS and not EPERM: runc inside the guest skips its session keyring on ENOSYS, so nested Docker still starts.
	nosys := uint(unix.ENOSYS)

	return &specs.LinuxSeccomp{
		DefaultAction: specs.ActAllow,
		Architectures: arches,
		Syscalls:      []specs.LinuxSyscall{{Names: keyringCalls, Action: specs.ActErrno, ErrnoRet: &nosys}},
	}, nil
}

// nativeArches names the compat ABIs too, because a filter kills every call from an arch it does not list.
func nativeArches(goarch string) ([]specs.Arch, error) {
	switch goarch {
	case "amd64":
		return []specs.Arch{specs.ArchX86_64, specs.ArchX86, specs.ArchX32}, nil
	case "arm64":
		return []specs.Arch{specs.ArchAARCH64, specs.ArchARM}, nil
	default:
		return nil, fmt.Errorf("no seccomp profile for %s: shard filters the keyring on amd64 and arm64 only", goarch)
	}
}
