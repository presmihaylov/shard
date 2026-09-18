// Package mountinfo reads the mounts of this process back from the kernel.
package mountinfo

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

const path = "/proc/self/mountinfo"

// Mount is what the kernel reports for the effective mount at one point.
type Mount struct {
	Point        string
	FSType       string
	Source       string
	SuperOptions string
}

// At asks the kernel what is mounted at point, because a shard restart forgets what it mounted.
func At(point string) (Mount, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Mount{}, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	return parse(f, point)
}

// parse keeps the last line at point: a later mount shadows an earlier one at the same point.
func parse(r io.Reader, point string) (Mount, bool, error) {
	var m Mount
	found := false

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		// Field 5 is the mount point, and the fields before it never contain a space.
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || unescape(fields[4]) != point {
			continue
		}

		// The optional fields are variable in number, so a lone "-" is what separates them from the rest.
		sep := indexSeparator(fields)
		if sep < 0 || len(fields) < sep+4 {
			return Mount{}, false, fmt.Errorf("%s has an unreadable line for %s", path, point)
		}

		m = Mount{Point: point, FSType: fields[sep+1], Source: unescape(fields[sep+2]), SuperOptions: unescape(fields[sep+3])}
		found = true
	}

	if err := scanner.Err(); err != nil {
		return Mount{}, false, fmt.Errorf("read %s: %w", path, err)
	}

	return m, found, nil
}

// The optional fields start at index 6, so an earlier field that happens to be "-" is not the separator.
func indexSeparator(fields []string) int {
	const optionalFieldsStart = 6

	if len(fields) < optionalFieldsStart {
		return -1
	}

	sep := slices.Index(fields[optionalFieldsStart:], "-")
	if sep < 0 {
		return -1
	}

	return optionalFieldsStart + sep
}

// The kernel octal-escapes space, tab, newline and backslash in a path.
var escapes = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func unescape(field string) string {
	return escapes.Replace(field)
}
