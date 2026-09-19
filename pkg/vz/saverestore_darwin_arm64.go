//go:build darwin && arm64

package vz

import (
	"fmt"

	"github.com/Code-Hex/vz/v3"
)

// The split follows hypeman's cmd/vz-shim/save_restore_arm64.go (561e34fd): only arm64 has the framework calls.
func checkSaveRestore(vmc *vz.VirtualMachineConfiguration) error {
	ok, err := vmc.ValidateSaveRestoreSupport()
	if err != nil {
		return fmt.Errorf("validate save and restore: %w", err)
	}
	if !ok {
		return fmt.Errorf("this vm configuration cannot be saved or restored")
	}

	return nil
}

func save(vm *vz.VirtualMachine, path string) error {
	return vm.SaveMachineStateToPath(path)
}

func restore(vm *vz.VirtualMachine, path string) error {
	return vm.RestoreMachineStateFromURL(path)
}
