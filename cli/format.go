package cli

import (
	"errors"
	"flag"
)

// NotImplementedExitCode is the one code every verb, flag or format that holds its final shape before its work lands exits with.
const NotImplementedExitCode = 3

// notImplemented is the refusal of such a verb; what is its full path, as snapshot create or list --format json.
func notImplemented(what string) error {
	return &ExitError{Code: NotImplementedExitCode, Message: what + ": not implemented yet"}
}

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

// parseFormatArgs is parseArgs for a verb whose only flag is --format.
func parseFormatArgs(verb string, args []string, def outputFormat) ([]string, outputFormat, error) {
	flags := newFlags(verb)
	format := addFormatFlag(flags, def)
	if err := parseVerb(flags, args); err != nil {
		return nil, "", err
	}

	return flags.Args(), *format, nil
}

// formatLanded refuses a format the verb does not write yet (SHARD-467); built is the one it does.
func formatLanded(verb string, format, built outputFormat) error {
	if format == built {
		return nil
	}

	return notImplemented(verb + " --format " + string(format))
}
