package firecracker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnlyAVersionFrom113OnPasses(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"Firecracker v1.17.0\n\nSupported snapshot data format versions: v7.0.0\n", true},
		{"Firecracker v1.13.0\n", true},
		{"Firecracker v2.0.1\n", true},
		{"Firecracker v1.12.1\n", false},
		{"Firecracker v1.9.0\n", false},
		{"Firecracker v0.25.2\n", false},
	}
	for _, c := range cases {
		v, err := parseVersion(c.out)
		if err != nil {
			t.Fatalf("parseVersion(%q) = %v", c.out, err)
		}
		if got := !v.older(minVersion); got != c.want {
			t.Errorf("%q passes = %t, want %t", c.out, got, c.want)
		}
	}
	if _, err := parseVersion("jailer v1.17.0\n"); err == nil {
		t.Error("parseVersion of a line with no firecracker version = nil, want an error")
	}
}

func TestCheckVersionRefusesAnOlderBinaryByItsVersion(t *testing.T) {
	old := script(t, "Firecracker v1.12.1")
	err := CheckVersion(old)
	if err == nil || !strings.Contains(err.Error(), "1.12.1") || !strings.Contains(err.Error(), "1.13.0") {
		t.Fatalf("CheckVersion of a 1.12.1 binary = %v, want a refusal that names both versions", err)
	}
	if err := CheckVersion(script(t, "Firecracker v1.17.0")); err != nil {
		t.Fatalf("CheckVersion of a 1.17.0 binary = %v, want nil", err)
	}
}

// script is a binary that answers --version with line.
func script(t *testing.T, line string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "firecracker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+line+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}
