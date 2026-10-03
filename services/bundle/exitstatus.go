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
	"github.com/presmihaylov/shard/pkg/store"
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

	exit, found, err := decodeExitRecord(blob)
	if err != nil {
		return models.ExitStatus{}, false, fmt.Errorf("decode the exit report in %s: %w", path, err)
	}

	return exit, found, nil
}

// DecodeExitPage reads the record off a sealed exit channel page; anything else there is a guest write, so it is no exit.
func DecodeExitPage(page []byte) (models.ExitStatus, bool) {
	if end := bytes.IndexByte(page, 0); end >= 0 {
		page = page[:end]
	}

	exit, found, err := decodeExitRecord(page)
	if err != nil {
		return models.ExitStatus{}, false
	}

	return exit, found
}

// WriteExitStatus replaces the exit file with one record, framed as shard-init frames it, so ReadExitStatus reads it.
func WriteExitStatus(path string, exit models.ExitStatus) error {
	encoded, err := json.Marshal(models.ExitReport{Kind: models.ExitReportKind, Code: exit.Code, Signal: exit.Signal})
	if err != nil {
		return fmt.Errorf("marshal the exit report: %w", err)
	}
	if err := store.WriteFile(path, append(append([]byte{'\n'}, encoded...), '\n'), 0o600); err != nil {
		return fmt.Errorf("write the exit record to %s: %w", path, err)
	}

	return nil
}

func decodeExitRecord(blob []byte) (models.ExitStatus, bool, error) {
	line := lastCompleteLine(blob)
	if line == nil {
		return models.ExitStatus{}, false, nil
	}

	var report models.ExitReport
	if err := json.Unmarshal(line, &report); err != nil {
		return models.ExitStatus{}, false, err
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
