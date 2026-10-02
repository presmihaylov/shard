package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"

	"github.com/presmihaylov/shard/models"
)

// exitFileCap bounds the read: shard-init keeps one record of a few dozen bytes, and sysbox guest root can append to the file.
const exitFileCap = 4 << 10

// ReadExitStatus reads shard-init's exit channel: the last complete newline-framed record, or not found.
func ReadExitStatus(path string) (models.ExitStatus, bool, error) {
	blob, err := readExitFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return models.ExitStatus{}, false, nil
	}
	if err != nil {
		return models.ExitStatus{}, false, err
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

// readExitFile empties a file past the cap, so a guest that writes to it cannot fill the host between two reads.
func readExitFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open the exit file: %w", err)
	}
	defer f.Close()

	blob, err := io.ReadAll(io.LimitReader(f, exitFileCap+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(blob) <= exitFileCap {
		return blob, nil
	}

	if err := os.Truncate(path, 0); err != nil {
		return nil, fmt.Errorf("empty the exit file %s: %w", path, err)
	}

	return nil, fmt.Errorf("%s was over %d bytes, and the host emptied it: %w", path, exitFileCap, models.ErrExitFileTooLarge)
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
