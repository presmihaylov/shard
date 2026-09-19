//go:build darwin && !arm64

package vz

import "github.com/Code-Hex/vz/v3"

func checkSaveRestore(*vz.VirtualMachineConfiguration) error { return ErrUnsupported }

func save(*vz.VirtualMachine, string) error { return ErrUnsupported }

func restore(*vz.VirtualMachine, string) error { return ErrUnsupported }
