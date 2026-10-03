package bundle

import (
	"slices"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// A filter kills every call from an arch it does not list, so a 32-bit binary in the guest would die on its first call.
func TestNativeArchesNameTheCompatABIs(t *testing.T) {
	for goarch, want := range map[string][]specs.Arch{
		"amd64": {specs.ArchX86_64, specs.ArchX86, specs.ArchX32},
		"arm64": {specs.ArchAARCH64, specs.ArchARM},
	} {
		got, err := nativeArches(goarch)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("nativeArches(%s) = %v, %v, want %v", goarch, got, err, want)
		}
	}

	if _, err := nativeArches("riscv64"); err == nil {
		t.Error("nativeArches(riscv64) gave a profile, want a refusal")
	}
}
