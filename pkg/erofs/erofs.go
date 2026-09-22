// Package erofs drives mkfs.erofs, which lays a directory down as one read-only EROFS image.
package erofs

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Tool is the binary Build runs; erofs-utils ships it.
const Tool = "mkfs.erofs"

// blockSize pins the image to 4 KiB blocks, whatever the page size of the host that builds it, so a 4 KiB-page guest mounts it.
const blockSize = "4096"

// Build writes dir as an EROFS image at dst, owners and xattrs included, so it runs over a tree unpacked as root.
func Build(ctx context.Context, dst, dir string) error {
	out, err := exec.CommandContext(ctx, Tool, "-b", blockSize, dst, dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("erofs: %s %s: %w: %s", Tool, dst, err, bytes.TrimSpace(out))
	}

	return nil
}
