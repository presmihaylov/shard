package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/presmihaylov/shard/models"
)

// RestartCount reads what shard-init kept of its restart policy on this run: zero before the first start again.
func (b Bundle) RestartCount() (models.RestartCount, error) {
	blob, err := os.ReadFile(b.RestartFile)
	if errors.Is(err, os.ErrNotExist) {
		return models.RestartCount{}, nil
	}
	if err != nil {
		return models.RestartCount{}, fmt.Errorf("read the restart count: %w", err)
	}

	var count models.RestartCount
	if err := json.Unmarshal(blob, &count); err != nil {
		return models.RestartCount{}, fmt.Errorf("decode the restart count: %w", err)
	}

	return count, nil
}
