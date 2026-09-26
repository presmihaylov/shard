package bundle

import (
	"bytes"
	"fmt"
	"os"

	"github.com/presmihaylov/shard/pkg/cpio"
	"github.com/presmihaylov/shard/pkg/store"
)

// WriteInitrd packs shard-init as /init of a newc archive at dst, so the kernel runs it as PID 1 with nothing else in the initramfs.
func WriteInitrd(initPath, dst string) error {
	body, err := os.ReadFile(initPath)
	if err != nil {
		return fmt.Errorf("read shard-init: %w", err)
	}

	var archive bytes.Buffer
	w := cpio.New(&archive)
	if err := w.File("init", 0o755, body); err != nil {
		return fmt.Errorf("pack shard-init into the initrd: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish the initrd: %w", err)
	}
	if err := store.WriteFile(dst, archive.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write the initrd: %w", err)
	}

	return nil
}
