//go:build !linux

package netns

import "context"

func (m *Manager) addOwnedNamespace(context.Context, string, IDMapping) error { return ErrNotLinux }

func unpinUserns(string) error { return nil }
