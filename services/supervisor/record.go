package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

// AppendExit lands one exit record in the exit file, framed as on fd 0, so bundle.ReadExitStatus reads both alike.
func AppendExit(path string, exit models.ExitStatus) (err error) {
	report := models.ExitReport{Kind: models.ExitReportKind, Code: exit.Code, Signal: exit.Signal}
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal the exit report: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open the exit file: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close the exit file: %w", closeErr))
		}
	}()
	if _, err := f.Write(append(append([]byte{'\n'}, encoded...), '\n')); err != nil {
		return fmt.Errorf("append the exit record to %s: %w", path, err)
	}

	return nil
}

// WriteRestarts keeps the guest's restart count where bundle.RestartCount reads it.
func WriteRestarts(path string, count models.RestartCount) error {
	encoded, err := json.Marshal(count)
	if err != nil {
		return fmt.Errorf("marshal the restart count: %w", err)
	}
	if err := store.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write the restart count: %w", err)
	}

	return nil
}
