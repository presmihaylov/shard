package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		var out bytes.Buffer
		if err := run([]string{arg}, &out); err != nil {
			t.Fatalf("run(%q) returned error: %v", arg, err)
		}

		if got := strings.TrimSpace(out.String()); got != version {
			t.Errorf("run(%q) printed %q, want %q", arg, got, version)
		}
	}
}

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var out bytes.Buffer
	if err := run(nil, &out); err != nil {
		t.Fatalf("run(nil) returned error: %v", err)
	}

	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("run(nil) printed %q, want usage text", out.String())
	}
}
