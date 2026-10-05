package firecracker

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
)

// minVersion is the oldest firecracker whose snapshot load takes track_dirty_pages, which every Diff after a restore needs.
var minVersion = version{1, 13, 0}

type version [3]int

func (v version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

func (v version) older(than version) bool {
	for i := range v {
		if v[i] != than[i] {
			return v[i] < than[i]
		}
	}

	return false
}

var versionLine = regexp.MustCompile(`Firecracker v(\d+)\.(\d+)\.(\d+)`)

// CheckVersion refuses a firecracker binary older than 1.13.0, by the version the binary names itself.
func CheckVersion(binary string) error {
	out, err := exec.Command(binary, "--version").Output()
	if err != nil {
		return fmt.Errorf("read the version of %s: %w", binary, err)
	}

	return CheckVersionOutput(binary, out)
}

// CheckVersionOutput is CheckVersion over what binary --version already printed.
func CheckVersionOutput(binary string, out []byte) error {
	v, err := parseVersion(string(out))
	if err != nil {
		return fmt.Errorf("read the version of %s: %w", binary, err)
	}
	if v.older(minVersion) {
		return fmt.Errorf("%s is firecracker %s, and shard needs %s or newer, whose snapshot load keeps the dirty-page log on", binary, v, minVersion)
	}

	return nil
}

func parseVersion(out string) (version, error) {
	m := versionLine.FindStringSubmatch(out)
	if m == nil {
		return version{}, fmt.Errorf("no firecracker version in %q", out)
	}
	var v version
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return version{}, fmt.Errorf("version part %q: %w", m[i+1], err)
		}
		v[i] = n
	}

	return v, nil
}
