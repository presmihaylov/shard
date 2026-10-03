//go:build darwin

package supervisor_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/supervisor"
)

// Slow appends leave APFS blocks past the end of a log, so a rotation that keeps them lets du pass the bound (SHARD-420).
func TestARotationFreesTheBlocksPastTheEndOnAPFS(t *testing.T) {
	const (
		chunk = 32 << 10
		max   = 8 << 20
		block = 4 << 10
	)
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	log := &supervisor.FileLog{File: f, Cursor: filepath.Join(dir, "output.cursor"), Max: max}
	defer log.Close()

	b := make([]byte, chunk)
	for range max/chunk + 1 {
		if _, err := log.Write(b); err != nil {
			t.Fatal(err)
		}
		time.Sleep(4 * time.Millisecond)
	}

	info, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s has no block count", path+".1")
	}
	held := st.Blocks * 512
	if limit := (info.Size() + block - 1) / block * block; held > limit {
		t.Errorf("the rotated log holds %d bytes on disk for %d bytes of output, want at most %d", held, info.Size(), limit)
	}
}
