// Package registry talks to OCI registries: auth, manifest resolution and layer
// fetch. Transport only. The unpack and cache rules live in services/image.
//
// SHARD-10 fills this package in.
package registry
