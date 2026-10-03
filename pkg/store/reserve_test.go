package store

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteReserveAllocatesEveryBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".reserve")
	const size = 3<<20 + 512

	if err := WriteReserve(path, size); err != nil {
		t.Fatalf("WriteReserve: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != size {
		t.Errorf("the reserve holds %d bytes, want %d", info.Size(), size)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat of %s has no Stat_t", path)
	}
	if st.Blocks*512 < size {
		t.Errorf("the reserve takes %d bytes on disk, want at least %d: a sparse file gives nothing back", st.Blocks*512, size)
	}
}

func TestFreeAnswersTheSpaceOfTheDir(t *testing.T) {
	free, err := Free(t.TempDir())
	if err != nil {
		t.Fatalf("Free: %v", err)
	}
	if free <= 0 {
		t.Errorf("Free = %d, want the space a temp dir still has", free)
	}
}
