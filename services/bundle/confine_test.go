package bundle_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
)

// The keyring quota is per host uid, which every Sysbox sandbox shares, so no guest may spend it (SHARD-367).
func TestTheKeyringProfileDeniesOnlyTheKeyringWithENOSYS(t *testing.T) {
	got, err := bundle.KeyringProfile(&specs.Spec{})
	if err != nil {
		t.Fatalf("KeyringProfile: %v", err)
	}

	if got.DefaultAction != specs.ActAllow {
		t.Errorf("got the default action %s, want %s: Sysbox keeps a deny-list and rewrites an allow-list", got.DefaultAction, specs.ActAllow)
	}
	if len(got.Syscalls) != 1 {
		t.Fatalf("got %d rules, want the one keyring rule", len(got.Syscalls))
	}
	rule := got.Syscalls[0]
	if !slices.Equal(rule.Names, []string{"add_key", "keyctl", "request_key"}) || rule.Action != specs.ActErrno {
		t.Errorf("got the rule %+v, want an errno on add_key, keyctl and request_key", rule)
	}
	if rule.ErrnoRet == nil || *rule.ErrnoRet != uint(unix.ENOSYS) {
		t.Errorf("got the errno %v, want ENOSYS, which runc in the guest skips its keyring on", rule.ErrnoRet)
	}
	if len(got.Architectures) < 2 {
		t.Errorf("got the arches %v, want the native one and its compat ABIs", got.Architectures)
	}
}

func TestBuildWritesTheSeccompFilter(t *testing.T) {
	got := buildWith(t, bundle.WithSeccomp(bundle.KeyringProfile))

	if got.Linux.Seccomp == nil || len(got.Linux.Seccomp.Syscalls) != 1 {
		t.Errorf("got the filter %+v, want the keyring rule", got.Linux.Seccomp)
	}
}

func TestBuildWritesTheAppArmorProfile(t *testing.T) {
	got := buildWith(t, bundle.WithAppArmor(bundle.AppArmorProfile))

	if got.Process.ApparmorProfile != bundle.AppArmorProfile {
		t.Errorf("got the AppArmor profile %q, want %s", got.Process.ApparmorProfile, bundle.AppArmorProfile)
	}
}

// gVisor's sentry is the boundary there, so its bundles carry no host filter and no profile.
func TestBuildConfinesNothingUnlessAsked(t *testing.T) {
	got := buildWith(t)

	if got.Linux.Seccomp != nil || got.Process.ApparmorProfile != "" {
		t.Errorf("got the filter %+v and the profile %q, want neither", got.Linux.Seccomp, got.Process.ApparmorProfile)
	}
}

// Docker's profile keys off the capabilities, so the filter must see the spec they are already in.
func TestBuildHandsTheFilterTheFinishedSpec(t *testing.T) {
	var caps []string
	buildWith(t, bundle.WithSeccomp(func(s *specs.Spec) (*specs.LinuxSeccomp, error) {
		caps = s.Process.Capabilities.Bounding
		return &specs.LinuxSeccomp{DefaultAction: specs.ActAllow}, nil
	}))

	if !slices.Contains(caps, "CAP_CHOWN") {
		t.Errorf("the filter saw the capabilities %v, want the sandbox's", caps)
	}
}

func TestBuildFailsWhenTheFilterDoes(t *testing.T) {
	broken := errors.New("no profile")
	_, err := newServiceWith(t, bundle.WithSeccomp(func(*specs.Spec) (*specs.LinuxSeccomp, error) { return nil, broken })).
		Build(runspec.Resolve(newSpec(t), models.ImageConfig{}))

	if !errors.Is(err, broken) {
		t.Errorf("Build = %v, want the filter's error", err)
	}
}

func buildWith(t *testing.T, opts ...bundle.Option) specs.Spec {
	t.Helper()

	b, err := newServiceWith(t, opts...).Build(runspec.Resolve(newSpec(t), models.ImageConfig{}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var got specs.Spec
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(b.Dir, "config.json"))), &got); err != nil {
		t.Fatalf("the bundle config.json does not parse as a runtime spec: %v", err)
	}

	return got
}

func newServiceWith(t *testing.T, opts ...bundle.Option) *bundle.Service {
	t.Helper()

	svc, err := bundle.New(supervisorPath, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return svc
}
