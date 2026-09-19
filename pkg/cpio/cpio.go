// Package cpio writes the newc archive a Linux kernel unpacks as its initramfs, and nothing else.
package cpio

import (
	"fmt"
	"io"
	"os"
)

// Writer lays out one newc archive; Close writes the trailer the kernel stops at.
type Writer struct {
	w   io.Writer
	ino uint32
	err error
}

// New wraps w; the caller must Close the writer for the archive to be complete.
func New(w io.Writer) *Writer { return &Writer{w: w} }

// File adds one regular file at name with the given mode bits, owned by root.
func (c *Writer) File(name string, mode os.FileMode, body []byte) error {
	if c.err != nil {
		return c.err
	}
	c.ino++
	c.err = c.entry(name, 0o100000|uint32(mode.Perm()), body)

	return c.err
}

// Close writes the trailer entry; the archive is not one without it.
func (c *Writer) Close() error {
	if c.err != nil {
		return c.err
	}
	c.ino++
	c.err = c.entry("TRAILER!!!", 0, nil)

	return c.err
}

func (c *Writer) entry(name string, mode uint32, body []byte) error {
	// The fields are ino, mode, uid, gid, nlink, mtime, filesize, devmajor, devminor, rdevmajor, rdevminor, namesize, check.
	header := fmt.Sprintf("070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		c.ino, mode, 0, 0, 1, 0, len(body), 0, 0, 0, 0, len(name)+1, 0)
	if _, err := io.WriteString(c.w, header+name+"\x00"); err != nil {
		return fmt.Errorf("write the header of %s: %w", name, err)
	}
	if err := c.pad(len(header) + len(name) + 1); err != nil {
		return err
	}
	if _, err := c.w.Write(body); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}

	return c.pad(len(body))
}

// pad aligns the next header on four bytes, which the kernel's parser expects after a name and after a body.
func (c *Writer) pad(n int) error {
	if n%4 == 0 {
		return nil
	}
	if _, err := c.w.Write(make([]byte, 4-n%4)); err != nil {
		return fmt.Errorf("pad the archive: %w", err)
	}

	return nil
}
