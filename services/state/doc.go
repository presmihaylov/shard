// Package state is the sandbox record repository. It owns the on-disk layout
// under /var/lib/shard/{images,instances,snapshots}. Files only, no database.
//
// SHARD-14 fills this package in.
package state
