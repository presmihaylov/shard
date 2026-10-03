package bundle

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A host whose module is on and whose parser is missing would run every runc sandbox unconfined, so the daemon refuses it (SHARD-391).
func TestAppArmorRefusesAModuleThatIsOnWithNoParser(t *testing.T) {
	host := fakeAppArmor(t, "Y\n")
	host.lookPath = func(file string) (string, error) { return "", &exec.Error{Name: file, Err: exec.ErrNotFound} }

	_, err := host.options()

	if err == nil || !strings.Contains(err.Error(), "install the apparmor package") || !strings.Contains(err.Error(), "apparmor_parser") {
		t.Errorf("options = %v, want a refusal that names apparmor_parser and the package", err)
	}
	if len(host.loaded) != 0 {
		t.Errorf("loaded %v, want nothing", host.loaded)
	}
}

func TestAppArmorLoadsTheProfileAndConfinesUnderIt(t *testing.T) {
	host := fakeAppArmor(t, "Y\n")

	opts, err := host.options()
	if err != nil {
		t.Fatalf("options: %v", err)
	}

	if len(host.loaded) != 1 || host.loaded[0] != AppArmorProfile {
		t.Errorf("loaded %v, want only %s", host.loaded, AppArmorProfile)
	}
	var s Service
	for _, opt := range opts {
		opt(&s)
	}
	if s.apparmor != AppArmorProfile {
		t.Errorf("got the profile %q, want %s", s.apparmor, AppArmorProfile)
	}
}

func TestAppArmorRefusesWhenTheLoadFails(t *testing.T) {
	broken := errors.New("apparmor_parser: no securityfs")
	host := fakeAppArmor(t, "Y\n")
	host.load = func(string) error { return broken }

	if _, err := host.options(); !errors.Is(err, broken) {
		t.Errorf("options = %v, want the load's error", err)
	}
}

// A host with no AppArmor has nothing to downgrade from, as with Docker.
func TestAppArmorConfinesNothingWhereTheModuleIsOff(t *testing.T) {
	for name, enabled := range map[string]string{"off": "N\n", "absent": ""} {
		t.Run(name, func(t *testing.T) {
			host := fakeAppArmor(t, enabled)

			opts, err := host.options()

			if err != nil || len(opts) != 0 || len(host.loaded) != 0 {
				t.Errorf("options = %d options, %v, loaded %v, want none", len(opts), err, host.loaded)
			}
		})
	}
}

type fakeHost struct {
	appArmorHost

	loaded []string
}

// fakeAppArmor is a host whose module parameter says enabled, or has no module when enabled is empty, with the parser on PATH.
func fakeAppArmor(t *testing.T, enabled string) *fakeHost {
	t.Helper()

	sys := t.TempDir()
	if enabled != "" {
		dir := filepath.Join(sys, "module", "apparmor", "parameters")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "enabled"), []byte(enabled), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	host := &fakeHost{}
	host.appArmorHost = appArmorHost{
		sys:      sys,
		lookPath: func(file string) (string, error) { return "/usr/sbin/" + file, nil },
		load: func(profile string) error {
			host.loaded = append(host.loaded, profile)

			return nil
		},
	}

	return host
}
