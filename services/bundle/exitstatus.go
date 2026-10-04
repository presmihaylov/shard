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
	"syscall"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/launch"
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

// ReadNotStarted answers the refusal shard-init recorded when the entrypoint's exec failed, or nil when there is none.
func ReadNotStarted(sandbox, path string) (*models.CommandNotStartedError, error) {
	blob, err := readExitFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	report, found, err := decodeReport(blob, models.NotStartedReportKind)
	if err != nil {
		return nil, fmt.Errorf("decode the not-started report in %s: %w", path, err)
	}
	if !found {
		return nil, nil
	}

	return notStarted(sandbox, report), nil
}

// DecodeNotStartedPage reads that refusal off a sealed exit channel page.
func DecodeNotStartedPage(sandbox string, page []byte) *models.CommandNotStartedError {
	if end := bytes.IndexByte(page, 0); end >= 0 {
		page = page[:end]
	}

	report, found, err := decodeReport(page, models.NotStartedReportKind)
	if err != nil || !found {
		return nil
	}

	return notStarted(sandbox, report)
}

// notStarted takes only the errno from the record, so the reason is the kernel's words and the code a shell's.
func notStarted(sandbox string, report models.ExitReport) *models.CommandNotStartedError {
	failed := &launch.NotStartedError{Errno: syscall.Errno(report.Errno)}
	code := models.CommandNotExecutableExitCode
	if failed.NotFound() {
		code = models.CommandNotFoundExitCode
	}

	return &models.CommandNotStartedError{Sandbox: sandbox, Reason: failed.Reason(), Code: code}
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
	report, found, err := decodeReport(blob, models.ExitReportKind)
	if err != nil || !found {
		return models.ExitStatus{}, false, err
	}

	return models.ExitStatus{Code: report.Code, Signal: report.Signal}, true, nil
}

// decodeReport answers the last complete line when it is of kind; a foreign or torn line is none, so the reader waits rather than believe it.
func decodeReport(blob []byte, kind string) (models.ExitReport, bool, error) {
	line := lastCompleteLine(blob)
	if line == nil {
		return models.ExitReport{}, false, nil
	}

	var report models.ExitReport
	if err := json.Unmarshal(line, &report); err != nil {
		return models.ExitReport{}, false, err
	}
	if report.Kind != kind {
		return models.ExitReport{}, false, nil
	}

	return report, true, nil
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
