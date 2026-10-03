// Package xfs drives mkfs.xfs and mount for one loopback image, the way the daemon provisions a reflink root.
package xfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Mkfs is the binary xfsprogs ships; a host without it cannot format an image.
const Mkfs = "mkfs.xfs"

// magic is the first four bytes of every xfs superblock.
const magic = "XFSB"

// stagingSuffix names the file a format works in until it succeeds.
const stagingSuffix = ".part"

// ErrNotLinux is what an image answers off Linux: fallocate, loop devices and mkfs.xfs live nowhere else.
var ErrNotLinux = errors.New("xfs: linux only")

// ErrNotImage is a file at the image path that mkfs.xfs never wrote; the daemon must not format over it.
var ErrNotImage = errors.New("not an xfs image")

// ErrFstabConflict is a line at the mount point that is not this loop mount; the next boot would mount that instead.
var ErrFstabConflict = errors.New("fstab already mounts the point from another source")

// Have reports whether mkfs.xfs is on PATH, and names the package when it is not.
func Have() error {
	if _, err := exec.LookPath(Mkfs); err != nil {
		return fmt.Errorf("%s is not on PATH; install xfsprogs", Mkfs)
	}

	return nil
}

// MakeImage reserves size bytes at path and formats them with reflink on; an image that exists is kept as it is.
func MakeImage(ctx context.Context, path string, size int64) error {
	formatted, err := IsImage(path)
	if err != nil {
		return err
	}
	if formatted {
		return nil
	}

	// The work lands under a staging name, so a crash or a failed mkfs never leaves an unformatted file at path.
	staging := path + stagingSuffix
	if err := os.Remove(staging); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", staging, err)
	}
	if err := reserve(staging, size); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, Mkfs, "-q", "-m", "reflink=1", staging).CombinedOutput(); err != nil {
		return errors.Join(fmt.Errorf("%s %s: %w: %s", Mkfs, staging, err, bytes.TrimSpace(out)), os.Remove(staging))
	}
	if err := os.Rename(staging, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", staging, path, err)
	}

	return nil
}

// Room is the bytes a new image at path can take: the free space beside it, plus the staging file of a crashed format, which MakeImage removes first.
func Room(path string) (int64, error) {
	return room(path)
}

// IsImage reports whether path holds an xfs superblock; a missing path is not an image, another file is an error.
func IsImage(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	head := make([]byte, len(magic))
	n, err := f.Read(head)
	if err != nil && n < len(magic) {
		return false, fmt.Errorf("%s: %w", path, ErrNotImage)
	}
	if string(head[:n]) != magic {
		return false, fmt.Errorf("%s: %w", path, ErrNotImage)
	}

	return true, nil
}

// Mount attaches the image over a loop device at point.
func Mount(ctx context.Context, image, point string) error {
	if out, err := exec.CommandContext(ctx, "mount", "-t", "xfs", "-o", "loop", image, point).CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s at %s: %w: %s", image, point, err, bytes.TrimSpace(out))
	}

	return nil
}

// FstabPath is where the line goes; a test points it at a file of its own.
var FstabPath = "/etc/fstab"

// fstabEscape encodes the characters getmntent reads as field separators, so a path with a space stays one field.
var fstabEscape = strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`)

// fstabUnescape reverses fstabEscape, so InFstab matches a stored path against the real one.
var fstabUnescape = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

// Fstab adds the line that mounts image at point on boot, once, with nofail; an old line that lacks nofail is upgraded in place, and a line that mounts point from anything else is a conflict.
func Fstab(image, point string) error {
	scan, err := scanFstab(image, point)
	if err != nil {
		return err
	}
	if scan.index < 0 {
		return appendFstab(image, point)
	}
	if !scan.ours {
		return fmt.Errorf("%s: %w: %q", FstabPath, ErrFstabConflict, strings.Join(strings.Fields(scan.lines[scan.index]), " "))
	}
	if scan.nofail {
		return nil
	}

	return rewriteNofail(scan.lines, scan.index)
}

// MigrateFstab upgrades an old data-disk line in place to carry nofail, so a host set up before SHARD-353 survives a missing disk at the next boot; it adds nothing when no such line is there.
func MigrateFstab(image, point string) error {
	scan, err := scanFstab(image, point)
	// A host without fstab has nothing to repair, so a missing file is not an error here.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if scan.index < 0 || !scan.ours || scan.nofail {
		return nil
	}

	return rewriteNofail(scan.lines, scan.index)
}

// appendFstab writes a fresh line that mounts image at point with nofail, on its own line at the end of fstab.
func appendFstab(image, point string) error {
	f, err := os.OpenFile(FstabPath, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", FstabPath, err)
	}
	// A last line with no trailing newline would glue our line onto it and break both.
	lead, err := fstabLead(f)
	if err != nil {
		return errors.Join(err, f.Close())
	}
	// nofail keeps a missing or broken image from stopping the boot in emergency mode.
	if _, err := fmt.Fprintf(f, "%s%s %s xfs loop,nofail 0 0\n", lead, fstabEscape.Replace(image), fstabEscape.Replace(point)); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", FstabPath, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", FstabPath, err), f.Close())
	}

	return f.Close()
}

// rewriteNofail adds nofail to the options of the managed line at idx and writes fstab back, leaving every other line as it was.
func rewriteNofail(lines []string, idx int) error {
	fields := strings.Fields(lines[idx])
	fields[3] = strings.Join(append(strings.Split(fields[3], ","), "nofail"), ",")
	lines[idx] = strings.Join(fields, " ")

	return writeFstab(lines)
}

// writeFstab replaces fstab through a staging file and a rename, so a crash never leaves the boot file half written.
func writeFstab(lines []string) error {
	info, err := os.Stat(FstabPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", FstabPath, err)
	}
	staging := FstabPath + stagingSuffix
	f, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("open %s: %w", staging, err)
	}
	if _, err := f.WriteString(strings.Join(lines, "\n")); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", staging, err), f.Close(), os.Remove(staging))
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", staging, err), f.Close(), os.Remove(staging))
	}
	if err := f.Close(); err != nil {
		return errors.Join(fmt.Errorf("close %s: %w", staging, err), os.Remove(staging))
	}
	if err := os.Rename(staging, FstabPath); err != nil {
		return errors.Join(fmt.Errorf("rename %s to %s: %w", staging, FstabPath, err), os.Remove(staging))
	}

	// Fsync the directory, or a power loss after the rename can lose it and leave the old fstab.
	return syncDir(filepath.Dir(FstabPath))
}

// syncDir flushes a directory's own metadata, so a rename or create in it survives a power loss.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", dir, err), d.Close())
	}

	return d.Close()
}

// fstabLead returns the newline our line needs first, so it never joins a file whose last line has none.
func fstabLead(f *os.File) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", FstabPath, err)
	}
	if info.Size() == 0 {
		return "", nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
		return "", fmt.Errorf("read the end of %s: %w", FstabPath, err)
	}
	if last[0] == '\n' {
		return "", nil
	}

	return "\n", nil
}

// InFstab reports whether our line already mounts point, and refuses a line that mounts it from another source, type or without loop.
func InFstab(image, point string) (bool, error) {
	scan, err := scanFstab(image, point)
	if err != nil {
		return false, err
	}
	if scan.index < 0 {
		return false, nil
	}
	if scan.ours {
		return true, nil
	}

	return false, fmt.Errorf("%s: %w: %q", FstabPath, ErrFstabConflict, strings.Join(strings.Fields(scan.lines[scan.index]), " "))
}

// fstabScan is the file split into lines and the first line that mounts the point, so a caller can add, upgrade or refuse it.
type fstabScan struct {
	lines  []string
	index  int  // -1 when no line mounts the point
	ours   bool // the line at index mounts point from image over loop, not another source
	nofail bool // and it already carries nofail
}

func scanFstab(image, point string) (fstabScan, error) {
	data, err := os.ReadFile(FstabPath)
	if err != nil {
		return fstabScan{}, fmt.Errorf("read %s: %w", FstabPath, err)
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") || fstabUnescape.Replace(fields[1]) != point {
			continue
		}
		ours := len(fields) >= 4 && fstabUnescape.Replace(fields[0]) == image && fields[2] == "xfs" && slices.Contains(strings.Split(fields[3], ","), "loop")
		nofail := ours && slices.Contains(strings.Split(fields[3], ","), "nofail")
		return fstabScan{lines: lines, index: i, ours: ours, nofail: nofail}, nil
	}

	return fstabScan{lines: lines, index: -1}, nil
}
