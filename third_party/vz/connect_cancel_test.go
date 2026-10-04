package vz

import (
	"runtime/cgo"
	"testing"
)

// Cancel keeps the handle valid for a late framework completion (SHARD-619).
func TestConnectCancelKeepsHandleForLateCompletion(t *testing.T) {
	managed := &managedConnect{fn: func(*VirtioSocketConnection, error) {
		t.Error("fn ran for a dial the caller cancelled")
	}}
	handle := cgo.NewHandle(managed)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("resolving the handle after cancel panicked: %v", r)
		}
	}()

	managed.cancel() // the caller timed out and gave up

	// connectionHandler's first act on a late completion; on the free-on-cancel code this panicked.
	got, ok := handle.Value().(*managedConnect)
	if !ok {
		t.Fatalf("handle resolved to %T; want *managedConnect", handle.Value())
	}
	if !got.dead.Load() {
		t.Error("cancel did not mark the dial dead, so a late callback would deliver to a gone caller")
	}
	handle.Delete() // the callback owns the delete
}
