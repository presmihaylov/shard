// Package size reads and writes the sizes a flag takes, in whole MiB.
package size

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// MiB is the unit the API and every record count a size in.
const MiB = 1 << 20

// units is the bytes in each suffix a size takes: binary for iB, decimal otherwise (SHARD-459).
var units = map[string]int64{"KiB": 1 << 10, "MiB": MiB, "GiB": 1 << 30, "KB": 1e3, "MB": 1e6, "GB": 1e9}

// ParseMiB reads a size as whole MiB: a suffix converts, a part of a MiB rounds up, and only 0 goes without a unit (SHARD-469).
func ParseMiB(value string) (int64, error) {
	if strings.HasPrefix(value, "-") {
		return 0, errors.New("want a size that is not negative")
	}
	digits := strings.TrimLeft(value, "0123456789")
	number, suffix := value[:len(value)-len(digits)], digits
	if number == "" {
		return 0, errors.New("want a whole size such as 512MiB or 2GiB")
	}
	if strings.HasPrefix(suffix, ".") || strings.HasPrefix(suffix, ",") {
		return 0, errors.New("want a whole number; a fraction is never rounded")
	}
	unit, ok := units[suffix]
	if suffix != "" && !ok {
		return 0, fmt.Errorf("unknown unit %q; want KiB, MiB, GiB, KB, MB or GB", suffix)
	}
	if suffix == "" {
		if significant := strings.TrimLeft(number, "0"); significant != "" {
			return 0, fmt.Errorf("want a unit, such as %sMiB or 2GiB", significant)
		}

		return 0, nil
	}

	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is too large to count", value)
	}
	if unit == MiB {
		return n, nil
	}
	if n > math.MaxInt64/unit {
		return 0, fmt.Errorf("%s is too large to count", value)
	}

	bytes := n * unit
	whole := bytes / MiB
	if bytes%MiB != 0 {
		whole++
	}

	return whole, nil
}

// Format writes mib in GiB when it is whole GiB, else in MiB, so ParseMiB reads it back exactly.
func Format(mib int64) string {
	if mib != 0 && mib%1024 == 0 {
		return strconv.FormatInt(mib/1024, 10) + "GiB"
	}

	return strconv.FormatInt(mib, 10) + "MiB"
}

// Show is Format for a sentence, with a space before the unit.
func Show(mib int64) string {
	f := Format(mib)

	return f[:len(f)-3] + " " + f[len(f)-3:]
}
