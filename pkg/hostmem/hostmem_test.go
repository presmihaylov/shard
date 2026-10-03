package hostmem

import (
	"strings"
	"testing"
)

func TestParseMemTotal(t *testing.T) {
	got, err := parseMemTotal(strings.NewReader("MemTotal:       32662752 kB\nMemFree:         1048576 kB\n"))
	if err != nil {
		t.Fatalf("parseMemTotal: %v", err)
	}
	if want := int64(32662752) << 10; got != want {
		t.Fatalf("parseMemTotal = %d, want %d", got, want)
	}
}

func TestParseMemTotalRefusesAFileWithout(t *testing.T) {
	if _, err := parseMemTotal(strings.NewReader("MemFree: 1048576 kB\n")); err == nil {
		t.Fatal("parseMemTotal took a file with no MemTotal")
	}
	if _, err := parseMemTotal(strings.NewReader("MemTotal: lots kB\n")); err == nil {
		t.Fatal("parseMemTotal took a MemTotal that is not a number")
	}
}

func TestTotalReadsThisHost(t *testing.T) {
	got, err := Total()
	if err != nil {
		t.Fatalf("Total: %v", err)
	}
	if got <= 0 {
		t.Fatalf("Total = %d, want more than zero", got)
	}
}
