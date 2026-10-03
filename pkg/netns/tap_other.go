//go:build !linux

package netns

import "fmt"

// ChownTapIn has no tun driver to ask off Linux.
func ChownTapIn(_, name string, _, _ int) error {
	return fmt.Errorf("give tap %s an owner: %w", name, ErrNotLinux)
}
