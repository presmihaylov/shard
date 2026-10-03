package cli

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// mib is the unit the API and every record count a size in.
const mib = 1 << 20

// sizeUnits is the bytes in each suffix a size flag takes: binary for iB, decimal otherwise (SHARD-459).
var sizeUnits = map[string]int64{"KiB": 1 << 10, "MiB": mib, "GiB": 1 << 30, "KB": 1e3, "MB": 1e6, "GB": 1e9}

// parseMiB reads a size as whole MiB: a suffix converts, a part of a MiB rounds up, and only 0 goes without a unit (SHARD-469).
func parseMiB(value string) (int64, error) {
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
	unit, ok := sizeUnits[suffix]
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
	if unit == mib {
		return n, nil
	}
	if n > math.MaxInt64/unit {
		return 0, fmt.Errorf("%s is too large to count", value)
	}

	bytes := n * unit
	whole := bytes / mib
	if bytes%mib != 0 {
		whole++
	}

	return whole, nil
}

// sizeMiB is a size flag that takes a unit suffix and keeps whole MiB, so --memory and --disk share one parser.
type sizeMiB struct{ mib *int64 }

func (s sizeMiB) String() string {
	if s.mib == nil {
		return "0"
	}

	return strconv.FormatInt(*s.mib, 10)
}

func (s sizeMiB) Set(value string) error {
	n, err := parseMiB(value)
	if err != nil {
		return err
	}
	*s.mib = n

	return nil
}
