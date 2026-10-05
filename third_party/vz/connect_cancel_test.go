package vz

import (
	"testing"
)

// Cancel reclaims the dial's registry slot and marks it dead, so a guest that never answers leaks nothing and a late callback delivers to nobody (SHARD-619).
func TestConnectCancelReclaimsTheRegistrySlot(t *testing.T) {
	id := pendingConnectSeq.Add(1)
	managed := &managedConnect{id: id, fn: func(*VirtioSocketConnection, error) {
		t.Error("fn ran for a dial the caller cancelled")
	}}
	pendingConnects.Store(id, managed)

	managed.cancel() // the caller timed out and gave up

	if _, ok := pendingConnects.Load(id); ok {
		t.Error("cancel left the registry slot, so the never-answered dial leaks")
	}

	fn, dead := managed.take()
	if !dead {
		t.Error("cancel did not mark the dial dead, so a callback that still wins the slot would deliver to a gone caller")
	}
	if fn != nil {
		t.Error("cancel did not drop the callback, so the dial still retains it and the channel it closes over")
	}
}

// A late callback on a reclaimed slot resolves no value, so it never panics on a freed handle (SHARD-619).
func TestLateCallbackFindsNoRegistrySlot(t *testing.T) {
	id := pendingConnectSeq.Add(1)
	managed := &managedConnect{id: id, fn: func(*VirtioSocketConnection, error) {}}
	pendingConnects.Store(id, managed)

	managed.cancel()

	// connectionHandler's first act on a completion; a reclaimed slot must be a miss, not a freed-handle panic.
	if _, ok := pendingConnects.LoadAndDelete(id); ok {
		t.Error("a late callback still found the slot, so cancel did not reclaim it")
	}
}
