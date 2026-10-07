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

// exitFileCap bounds the read: shard-init keeps one table of at most the sealed page, and guest root can append to the file.
const exitFileCap = models.ExitChannelSize

// ReadProcessTable reads shard-init's status channel: the last complete table, or none before the first.
func ReadProcessTable(path string) ([]models.ProcessReport, error) {
	blob, err := readExitFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	table, err := decodeTable(lastCompleteLine(blob))
	if err != nil {
		return nil, fmt.Errorf("decode the process table in %s: %w", path, err)
	}

	return table, nil
}

// WriteProcessPage keeps a sealed status page in the exit file as the guest left it, cut at the NULs, so ReadProcessTable judges it as it judges a file.
func WriteProcessPage(path string, page []byte) error {
	if len(page) > models.ExitChannelSize {
		return fmt.Errorf("the status page is %d bytes, past the %d the channel holds", len(page), models.ExitChannelSize)
	}
	if end := bytes.IndexByte(page, 0); end >= 0 {
		page = page[:end]
	}
	if err := store.WriteFile(path, page, 0o600); err != nil {
		return fmt.Errorf("keep the status page in %s: %w", path, err)
	}

	return nil
}

// decodeTable takes a torn write, which has no complete line yet, as no table; any whole line but a valid table is a guest write.
func decodeTable(line []byte) ([]models.ProcessReport, error) {
	if line == nil {
		return nil, nil
	}

	var table models.ProcessTable
	if err := json.Unmarshal(line, &table); err != nil {
		return nil, fmt.Errorf("parse the record: %w", err)
	}
	if table.Kind != models.ProcessTableKind {
		return nil, fmt.Errorf("the record is of kind %q, want %q", table.Kind, models.ProcessTableKind)
	}
	if len(table.Processes) > models.MaxProcesses {
		return nil, fmt.Errorf("the table holds %d processes, past the %d a sandbox runs", len(table.Processes), models.MaxProcesses)
	}
	for _, report := range table.Processes {
		if !models.ValidProcessName(report.Name) {
			return nil, fmt.Errorf("the table names a process %q, which is no valid name", report.Name)
		}
	}

	return table.Processes, nil
}

// readExitFile empties a file past the cap, so a guest that writes to it cannot fill the host between two reads.
func readExitFile(path string) (_ []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open the exit file: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()

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

// lastCompleteLine returns the last newline-terminated non-empty line, so a write still in flight, which has no closing newline yet, is never read as a record.
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
