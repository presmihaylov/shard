package cli

import (
	"strconv"

	"github.com/presmihaylov/shard/pkg/size"
)

// sizeMiB is a size flag that takes a unit suffix and keeps whole MiB, so --memory and --disk share one parser.
type sizeMiB struct{ mib *int64 }

func (s sizeMiB) String() string {
	if s.mib == nil {
		return "0"
	}

	return strconv.FormatInt(*s.mib, 10)
}

func (s sizeMiB) Set(value string) error {
	n, err := size.ParseMiB(value)
	if err != nil {
		return err
	}
	*s.mib = n

	return nil
}

// optionalMiB is a size flag that stays nil until it is given, so an explicit 0 differs from an omitted flag.
type optionalMiB struct{ mib **int64 }

func (o optionalMiB) String() string {
	if o.mib == nil || *o.mib == nil {
		return ""
	}

	return strconv.FormatInt(**o.mib, 10)
}

func (o optionalMiB) Set(value string) error {
	n, err := size.ParseMiB(value)
	if err != nil {
		return err
	}
	*o.mib = &n

	return nil
}
