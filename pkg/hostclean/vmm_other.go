//go:build integration && !linux

package hostclean

// pinVMM takes no process as a vmm off Linux, where no firecracker vmm runs.
func pinVMM(int, string) (Leftover, bool, error) {
	return Leftover{}, false, nil
}
