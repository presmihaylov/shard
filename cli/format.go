package cli

import (
	"errors"
	"flag"
)

// outputFormat is what --format takes: the shape a verb writes its result to stdout in.
type outputFormat string

const (
	formatJSON  outputFormat = "json"
	formatTable outputFormat = "table"
)

func (f *outputFormat) String() string { return string(*f) }

func (f *outputFormat) Set(value string) error {
	if value != string(formatJSON) && value != string(formatTable) {
		return errors.New("want json or table")
	}
	*f = outputFormat(value)

	return nil
}

// addFormatFlag gives a verb --format, set to the shape it writes without one.
func addFormatFlag(flags *flag.FlagSet, def outputFormat) *outputFormat {
	format := def
	flags.Var(&format, "format", "")

	return &format
}

// formatSet says --format was typed rather than left at its default.
func formatSet(flags *flag.FlagSet) bool {
	set := false
	flags.Visit(func(f *flag.Flag) { set = set || f.Name == "format" })

	return set
}

// parseFormatArgs is parseArgs for a verb whose only flag is --format.
func parseFormatArgs(verb string, args []string, def outputFormat) ([]string, outputFormat, error) {
	flags := newFlags(verb)
	format := addFormatFlag(flags, def)
	if err := parseVerb(flags, args); err != nil {
		return nil, "", err
	}

	return flags.Args(), *format, nil
}
