package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

// WriteExit replaces the exit file with one record, framed as on fd 0, so bundle.ReadExitStatus reads both alike.
func WriteExit(path string, exit models.ExitStatus) (err error) {
	report := models.ExitReport{Kind: models.ExitReportKind, Code: exit.Code, Signal: exit.Signal}
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal the exit report: %w", err)
	}

	// O_TRUNC keeps one record however often the entrypoint restarts, under the reader's 4 KiB bound.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open the exit file: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close the exit file: %w", closeErr))
		}
	}()
	if _, err := f.Write(append(append([]byte{'\n'}, encoded...), '\n')); err != nil {
		return fmt.Errorf("write the exit record to %s: %w", path, err)
	}

	return nil
}

// WriteRestarts keeps the guest's restart count where bundle.RestartCount reads it.
func WriteRestarts(path string, count models.RestartCount) error {
	encoded, err := json.Marshal(count)
	if err != nil {
		return fmt.Errorf("marshal the restart count: %w", err)
	}
	if err := store.WriteFileIfChanged(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write the restart count: %w", err)
	}

	return nil
}

// maxReason bounds what a guest's reason may take of a record, a log line and a column of ls.
const maxReason = 256

// OneLine makes the guest's reason safe for a record and a log line: no control bytes, valid UTF-8, at most maxReason bytes.
func OneLine(reason string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}

		return r
	}, strings.ToValidUTF8(reason, "?"))
	if len(clean) <= maxReason {
		return clean
	}
	cut := maxReason
	for !utf8.RuneStart(clean[cut]) {
		cut--
	}

	return clean[:cut]
}
