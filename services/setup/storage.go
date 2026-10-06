package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/pkg/size"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/datadir"
)

// disk is the filesystem a path is on, as statfs reads it.
type disk struct {
	total, used, avail int64
	reflink            bool
}

// statDisk is the seam a test swaps, since a temp dir sits on whatever filesystem the machine has.
var statDisk = func(dir string) (disk, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return disk{}, err
	}
	bsize := int64(st.Bsize)

	//nolint:gosec // G115: a block count fits in int64 on any disk a host has
	return disk{
		total:   int64(st.Blocks) * bsize,
		used:    int64(st.Blocks-st.Bfree) * bsize,
		avail:   int64(st.Bavail) * bsize,
		reflink: int64(st.Type) == xfsMagic || int64(st.Type) == btrfsMagic,
	}, nil
}

const askStorage = "How much space should shard reserve?"

// storageSize asks for the size of the data image where Firecracker makes one; elsewhere it refuses --storage-size and names why.
func (s *Setup) storageSize(ctx context.Context, l Local) (Local, error) {
	h := s.Host
	if h.OS != "linux" || l.Provider != Firecracker {
		return l, s.refuseStorage(noReserve(l.Provider))
	}
	image := datadir.ImagePath(DataDir)
	bytes, found, err := imageBytes(h)
	if err != nil {
		return l, err
	}
	if found {
		if err := s.UI.Print("", fmt.Sprintf("Storage: %s in %s. Setup does not resize it.", gib(bytes), image)); err != nil {
			return l, err
		}

		return l, s.keepStorage(Firecracker, bytes)
	}

	root, err := statNearest(h, DataDir)
	if err != nil {
		return l, err
	}
	if root.reflink {
		return l, s.refuseStorage(noImage)
	}
	d, err := statNearest(h, filepath.Dir(image))
	if err != nil {
		return l, err
	}
	l.avail = d.avail
	if err := s.UI.Print(storageLines(l, d)...); err != nil {
		return l, err
	}
	if err := datadir.CheckSize(datadir.MinImageMiB, d.avail); err != nil {
		return l, s.stopStorage(sentence(err))
	}
	if s.StorageMiB != nil {
		if err := datadir.CheckSize(*s.StorageMiB, d.avail); err != nil {
			return l, s.refuseStorage(err.Error() + ".")
		}
		l.StorageMiB = *s.StorageMiB

		return l, nil
	}

	for {
		answer, err := s.UI.Text(ctx, AskStorage, askStorage, size.Format(datadir.DefaultMiB(d.avail)))
		if err != nil {
			return l, err
		}
		mib, err := size.ParseMiB(strings.TrimSpace(answer))
		if err == nil {
			err = datadir.CheckSize(mib, d.avail)
		}
		if err == nil {
			l.StorageMiB = mib

			return l, nil
		}
		if err := s.UI.Print(sentence(err), ""); err != nil {
			return l, err
		}
	}
}

// storageLines are what a person needs before choosing the size: the space there is, what the image is, and when it is taken.
func storageLines(l Local, d disk) []string {
	dir := filepath.Dir(datadir.ImagePath(DataDir))
	when := "when you first start the daemon"
	if l.StartAtBoot {
		when = "when setup starts the daemon"
	}

	return []string{
		"",
		"Sandbox storage",
		"",
		fmt.Sprintf("The filesystem of %s: %s in total, %s used, %s available.", dir, gib(d.total), gib(d.used), gib(d.avail)),
		"It cannot clone a disk, so Firecracker keeps sandbox disks in an XFS image, " + datadir.ImagePath(DataDir) + ".",
		"shard reserves the whole size at once, " + when + ". The host cannot use that space, even while sandboxes leave it empty.",
		fmt.Sprintf("Each Firecracker sandbox has a %s disk limit by default. Choose a smaller limit with shard create --disk.", size.Show(bundle.DefaultDiskMiB)),
		fmt.Sprintf("Enter a size from %s to %s, in KiB, MiB, GiB, KB, MB or GB. The default is half the available space, at most 100 GiB.", size.Show(datadir.MinImageMiB), size.Show(datadir.MaxMiB(d.avail))),
		"",
	}
}

const noImage = DataDir + " is on a filesystem that clones a disk, so Firecracker makes no data image there and reserves no space."

func noReserve(provider string) string {
	return providerTitle(provider) + " reserves no space: it keeps sandbox data in " + DataDir + " as it grows."
}

// refuseStorage stops a run whose --storage-size cannot apply, before any change; a run without the flag goes on.
func (s *Setup) refuseStorage(reason string) error {
	if s.StorageMiB == nil {
		return nil
	}

	return s.stopStorage("--storage-size " + size.Format(*s.StorageMiB) + ": " + reason)
}

func (s *Setup) stopStorage(line string) error {
	if err := s.UI.Print(line, "", "No installation changes were made."); err != nil {
		return err
	}

	return &StoppedError{Step: "Choose the storage size", Err: &Problem{Lines: []string{line}}}
}

// keepStorage refuses a --storage-size other than what an installation of provider reserves already, in bytes; an image made before the question is no whole MiB, so a part rounds up as ParseMiB rounds it.
func (s *Setup) keepStorage(provider string, bytes int64) error {
	switch {
	case s.StorageMiB == nil || bytes > 0 && *s.StorageMiB == (bytes+size.MiB-1)/size.MiB:
		return nil
	case provider != Firecracker:
		return s.refuseStorage(noReserve(provider))
	case bytes > 0:
		return s.refuseStorage(fmt.Sprintf("this installation already reserves %s, and setup does not resize it.", gib(bytes)))
	}
	root, err := statNearest(s.Host, DataDir)
	if err != nil {
		return err
	}
	if root.reflink {
		return s.refuseStorage(noImage)
	}

	return s.refuseStorage("this installation has no data image yet, and setup does not change the storage of an installation.")
}

// existingStorage is the Storage line of an installation's summary, empty where it reserves nothing, and the bytes it reserves.
func existingStorage(h Host, m Manifest) (string, int64, error) {
	if m.Provider != Firecracker {
		return "", 0, nil
	}
	bytes, found, err := imageBytes(h)
	if err != nil || found {
		return "Storage:  " + gib(bytes) + " in " + datadir.ImagePath(DataDir), bytes, err
	}
	if m.StorageMiB == 0 {
		return "", 0, nil
	}

	return "Storage:  " + gib(m.StorageMiB<<20) + ", reserved when the daemon first starts", m.StorageMiB << 20, nil
}

// imageBytes is the size of the data image already on the host, and whether there is one.
func imageBytes(h Host) (int64, bool, error) {
	image := datadir.ImagePath(DataDir)
	info, err := os.Stat(rooted(h, image))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("check %s: %w", image, err)
	}

	return info.Size(), true, nil
}

// statNearest reads the filesystem of dir, or of the closest directory above it that exists.
func statNearest(h Host, dir string) (disk, error) {
	near, err := nearestDir(rooted(h, dir))
	if err != nil {
		return disk{}, fmt.Errorf("check %s: %w", dir, err)
	}
	d, err := statDisk(near)
	if err != nil {
		return disk{}, fmt.Errorf("read the filesystem of %s: %w", dir, err)
	}

	return d, nil
}

// roomAtStart checks a chosen size again just before the daemon reserves it, since the disk may have filled after the question.
func roomAtStart(h Host, mib int64) error {
	if mib == 0 {
		return nil
	}
	_, found, err := imageBytes(h)
	if err != nil || found {
		return err
	}
	d, err := statNearest(h, filepath.Dir(datadir.ImagePath(DataDir)))
	if err != nil {
		return err
	}
	if err := datadir.CheckSize(mib, d.avail); err != nil {
		return &Problem{Lines: []string{"shard cannot reserve " + size.Show(mib) + " now: " + err.Error() + "."}}
	}

	return nil
}
