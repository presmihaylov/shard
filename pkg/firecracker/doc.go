// Package firecracker drives the firecracker binary and its API socket.
//
// Its name collides with services/provider/firecracker. That is legal, but a
// file that imports both must alias one. Alias this one, as fcapi.
//
// Chunk 5 fills this package in.
package firecracker
