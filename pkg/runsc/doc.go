// Package runsc drives the runsc binary: create, start, wait, kill, delete,
// pause, resume, checkpoint and restore. Bare runsc, no Docker and no
// containerd, because gVisor checkpoint/restore is unreachable through the
// containerd shim (containerd#12280).
//
// SHARD-12 fills this package in.
package runsc
