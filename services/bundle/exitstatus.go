package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/presmihaylov/shard/models"
)

// ReadExitStatus reads shard-init's exit channel: the last complete newline-framed record, or not found.
func ReadExitStatus(path string) (models.ExitStatus, bool, error) {
	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return models.ExitStatus{}, false, nil
	}
	if err != nil {
		return models.ExitStatus{}, false, fmt.Errorf("read %s: %w", path, err)
	}

	line := lastCompleteLine(blob)
	if line == nil {
		return models.ExitStatus{}, false, nil
	}

	var report models.ExitReport
	if err := json.Unmarshal(line, &report); err != nil {
		return models.ExitStatus{}, false, fmt.Errorf("decode the exit report in %s: %w", path, err)
	}
	// A foreign or torn line is not an exit, so the reader waits rather than believe it.
	if report.Kind != models.ExitReportKind {
		return models.ExitStatus{}, false, nil
	}

	return models.ExitStatus{Code: report.Code, Signal: report.Signal}, true, nil
}

// lastCompleteLine returns the last newline-terminated non-empty line, so a write still in flight,
// which has no closing newline yet, is never read as a record.
func lastCompleteLine(blob []byte) []byte {
	end := bytes.LastIndexByte(blob, '\n')
	if end < 0 {
		return nil
	}

	lines := bytes.Split(blob[:end+1], []byte{'\n'})
	for _, candidate := range slices.Backward(lines) {
		if line := bytes.TrimSpace(candidate); len(line) > 0 {
			return line
		}
	}

	return nil
}
