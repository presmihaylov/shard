// Package store holds file primitives: atomic write, read and the lockfile that
// makes concurrent access safe. It knows nothing about sandboxes.
//
// Two processes share the state directory, because shard serve and the CLI both
// write it. Design for that, not for one writer.
//
// SHARD-14 fills this package in.
package store
