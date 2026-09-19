// Package vz drives Apple's Virtualization.framework through one shard-vz-shim process per VM, over a unix socket, and knows no sandbox.
package vz

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrUnsupported is what every verb returns off a Mac, and what save and restore return on an Intel one.
var ErrUnsupported = errors.New("vz: Virtualization.framework needs macOS on Apple silicon")

// Config is what the shim boots. It is the argument of the shim's -config flag, as JSON.
type Config struct {
	Kernel  string `json:"kernel"`
	Initrd  string `json:"initrd,omitempty"`
	Cmdline string `json:"cmdline"`
	// Zero keeps the framework's default; a value outside its range is refused, never clamped.
	CPUs   uint   `json:"cpus"`
	Memory uint64 `json:"memory"`
	Disk   string `json:"disk,omitempty"`
	// Network attaches one virtio-net device over a datagram socketpair the shim keeps the host end of.
	Network bool `json:"network"`
	// MachineID is the identifier a restore must match; empty means the shim makes one and reports it.
	MachineID string `json:"machine_id,omitempty"`
	// Restore names a saved state to boot from instead of a cold start.
	Restore string `json:"restore,omitempty"`
	Socket  string `json:"socket"`
	Console string `json:"console"`
}

// State is the VM's state as the framework reports it.
type State string

const (
	StateStopped   State = "stopped"
	StateRunning   State = "running"
	StatePaused    State = "paused"
	StateError     State = "error"
	StateStarting  State = "starting"
	StatePausing   State = "pausing"
	StateResuming  State = "resuming"
	StateStopping  State = "stopping"
	StateSaving    State = "saving"
	StateRestoring State = "restoring"
)

// Range is the framework's allowed span for cpus or memory, both ends inclusive.
type Range struct {
	Min uint64
	Max uint64
}

// CheckCPUs refuses an explicit count outside the range; zero asks for the default.
func CheckCPUs(cpus uint, allowed Range) error {
	if cpus == 0 || (uint64(cpus) >= allowed.Min && uint64(cpus) <= allowed.Max) {
		return nil
	}

	return fmt.Errorf("vz: %d cpus is outside the host's range of %d to %d", cpus, allowed.Min, allowed.Max)
}

// CheckMemory refuses an explicit size outside the range; zero asks for the default.
func CheckMemory(bytes uint64, allowed Range) error {
	if bytes == 0 || (bytes >= allowed.Min && bytes <= allowed.Max) {
		return nil
	}

	return fmt.Errorf("vz: %d bytes of memory is outside the host's range of %d to %d", bytes, allowed.Min, allowed.Max)
}

// saveRestoreMinMajor is the first macOS whose framework saves and restores a VM.
const saveRestoreMinMajor = 14

// SaveRestoreSupported fails closed: only darwin on arm64 with a parsable macOS 14 or later says yes.
func SaveRestoreSupported(goos, goarch, productVersion string) bool {
	if goos != "darwin" || goarch != "arm64" {
		return false
	}

	major, _, _ := strings.Cut(strings.TrimSpace(productVersion), ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return false
	}

	return n >= saveRestoreMinMajor
}
