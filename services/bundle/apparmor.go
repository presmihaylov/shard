package bundle

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// AppArmorProfile is the name shard loads Docker's default profile under, so it never replaces a Docker's docker-default on the same host.
const AppArmorProfile = "shard-default"

// WithAppArmor confines every bundle's process under profile, which the host must already have loaded.
func WithAppArmor(profile string) Option {
	return func(s *Service) { s.apparmor = profile }
}

// appArmorHost is what the AppArmor decision reads and runs, so a test can stand in for the host.
type appArmorHost struct {
	// sys is where sysfs is mounted.
	sys      string
	lookPath func(file string) (string, error)
	load     func(profile string) error
}

// options loads Docker's AppArmor profile where the module is on, and refuses a host that runs the module with no parser to load it.
func (h appArmorHost) options() ([]Option, error) {
	on, err := h.on()
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, nil
	}

	if _, err := h.lookPath("apparmor_parser"); err != nil {
		return nil, fmt.Errorf("the AppArmor module is on, so every runc sandbox runs under %s, but %w: install the apparmor package", AppArmorProfile, err)
	}
	if err := h.load(AppArmorProfile); err != nil {
		return nil, fmt.Errorf("load the %s AppArmor profile: %w", AppArmorProfile, err)
	}

	return []Option{WithAppArmor(AppArmorProfile)}, nil
}

// on reads the module parameter alone, so a host whose securityfs is missing fails the load rather than run unconfined.
func (h appArmorHost) on() (bool, error) {
	enabled, err := os.ReadFile(filepath.Join(h.sys, "module", "apparmor", "parameters", "enabled"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe AppArmor: %w", err)
	}

	return bytes.HasPrefix(enabled, []byte("Y")), nil
}
