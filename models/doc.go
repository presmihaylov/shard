// Package models holds the domain types: Sandbox, its state set and legal
// transitions, the Provider interface, Capabilities, ImageRef, Policy and
// Secret metadata.
//
// It is a leaf. It imports nothing else in this module, and it performs no I/O.
//
// It stays ONE package with several files. Splitting it per concern invites
// import cycles between the concerns, which Go refuses to compile.
//
// The Provider interface lives here rather than at its consumer, which is a
// deliberate break from Go idiom: both provider implementations and cli need it.
//
// SHARD-8 fills this package in.
package models
