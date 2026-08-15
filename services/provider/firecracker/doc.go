// Package firecracker implements models.Provider on Firecracker microVMs,
// driving pkg/firecracker. It is the KVM substrate and the second provider.
//
// Its job is to break a bad abstraction while repair is still cheap. SHARD-45
// revises the interface once this package is real.
//
// Chunk 5 fills this package in.
package firecracker
