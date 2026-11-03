package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseResizeKeepsEveryBoundItIsNotGivenNil(t *testing.T) {
	ref, req, err := parseResize([]string{"--memory", "2GiB", "--vcpus", "0", "web"})
	if err != nil {
		t.Fatalf("parseResize: %v", err)
	}

	res := req.Resources
	if ref != "web" || res.MemoryMiB == nil || *res.MemoryMiB != 2048 || res.VCPUs == nil || *res.VCPUs != 0 {
		t.Errorf("parseResize gave %q and %+v, want web, 2048 MiB and an explicit 0 vCPUs", ref, res)
	}
	if res.DiskMiB != nil || res.SwapMiB != nil {
		t.Errorf("parseResize set a bound it was not given: disk %v, swap %v", res.DiskMiB, res.SwapMiB)
	}
}

func TestParseResizeRejections(t *testing.T) {
	cases := map[string][]string{
		"no id":           {"--memory", "1GiB"},
		"two ids":         {"--memory", "1GiB", "sandbox1", "sandbox2"},
		"no bound":        {"sandbox1"},
		"a flag after id": {"sandbox1", "--memory", "1GiB"},
		"negative vcpus":  {"--vcpus", "-1", "sandbox1"},
		"an unknown flag": {"--fast", "sandbox1"},
	}

	for name, args := range cases {
		if _, _, err := parseResize(args); err == nil {
			t.Errorf("parseResize(%s) returned no error", name)
		}
	}
}

// The verb reaches the daemon over the wire, which holds a running sandbox's bounds as they are.
func TestResizeRefusesARunningSandboxOverTheWire(t *testing.T) {
	var out bytes.Buffer
	app, _ := newLifecycleApp(t, &out, &recorder{}, running())

	err := app.Run(t.Context(), []string{"resize", "--memory", "1GiB", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Fatalf("resize of a running sandbox returned %v, want a refusal that names the fix", err)
	}
	if out.Len() != 0 {
		t.Errorf("a refused resize printed %q", out.String())
	}
}

func TestResizeRefusesADiskGrowTheProviderLacksOverTheWire(t *testing.T) {
	var out bytes.Buffer
	app, _ := newLifecycleApp(t, &out, &recorder{}, stopped())

	err := app.Run(t.Context(), []string{"resize", "--disk", "20GiB", "web"})
	if err == nil || !strings.Contains(err.Error(), "does not support disk grow") {
		t.Fatalf("a disk grow on a provider without it returned %v, want a refusal that names the disk grow", err)
	}
}
