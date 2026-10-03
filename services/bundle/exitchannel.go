package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/presmihaylov/shard/pkg/store"
)

// ExitChannel names the sealed memfd a sysbox create handed PID 1, so a restarted daemon can tell it from a guest's own file.
type ExitChannel struct {
	Inode uint64 `json:"inode"`
}

// RecordExitChannel keeps the channel's identity at the state directory root, which no guest reaches.
func (b Bundle) RecordExitChannel(c ExitChannel) error {
	encoded, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal the exit channel: %w", err)
	}
	if err := store.WriteFile(b.ExitChannelFile, encoded, 0o600); err != nil {
		return fmt.Errorf("record the exit channel: %w", err)
	}

	return nil
}

// ExitChannel reads what RecordExitChannel kept; a sandbox created before the sealed channel has none.
func (b Bundle) ExitChannel() (ExitChannel, bool, error) {
	blob, err := os.ReadFile(b.ExitChannelFile)
	if errors.Is(err, fs.ErrNotExist) {
		return ExitChannel{}, false, nil
	}
	if err != nil {
		return ExitChannel{}, false, fmt.Errorf("read the exit channel record: %w", err)
	}

	var c ExitChannel
	if err := json.Unmarshal(blob, &c); err != nil {
		return ExitChannel{}, false, fmt.Errorf("decode the exit channel record %s: %w", b.ExitChannelFile, err)
	}

	return c, true, nil
}
