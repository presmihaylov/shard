package bundle_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/services/bundle"
)

func TestWriteInitrdPacksShardInitAsInit(t *testing.T) {
	init := filepath.Join(t.TempDir(), "shard-init")
	if err := os.WriteFile(init, []byte("#!/bin/sh\necho init\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "initrd.cpio")

	if err := bundle.WriteInitrd(init, dst); err != nil {
		t.Fatalf("WriteInitrd: %v", err)
	}

	archive, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(archive, []byte("070701")) {
		t.Errorf("the archive starts %q, not newc", archive[:6])
	}
	for _, want := range []string{"init\x00", "echo init", "TRAILER!!!"} {
		if !bytes.Contains(archive, []byte(want)) {
			t.Errorf("the archive lacks %q", want)
		}
	}
	if bytes.Contains(archive, []byte("shard-init")) {
		t.Error("the archive names the host file, not /init")
	}
}

func TestWriteInitrdNamesAMissingShardInit(t *testing.T) {
	err := bundle.WriteInitrd(filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "initrd.cpio"))
	if err == nil {
		t.Fatal("WriteInitrd packed a shard-init that is not there")
	}
}
