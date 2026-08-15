// Package egress compiles named policies and applies them, deny by default,
// across the two enforcement layers: host netfilter for raw TCP, and the proxy
// for HTTP and TLS.
//
// The egress event log is a product feature, not telemetry. Deny by default is
// only tolerable when a user can see what got denied and which rule did it.
//
// Chunk 4 fills this package in.
package egress
