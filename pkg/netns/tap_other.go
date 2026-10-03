//go:build !linux

package netns

import "fmt"

// ChownTap has no tun driver to ask off Linux.
func ChownTap(name string, _, _ int) error {
	return fmt.Errorf("give tap %s an owner: %w", name, ErrNotLinux)
}
