//go:build !linux

package vzvm_test

import "testing"

// awaitStopped has nothing to wait for: a darwin kill returns once task_suspend has held every thread.
func awaitStopped(*testing.T, int) {}
