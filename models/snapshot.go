package models

import "time"

// Snapshot is the files a stopped sandbox kept, with no memory; it outlives its source.
type Snapshot struct {
	ID string `json:"id"`
	// Name is unique among snapshots; sandbox names are a separate space.
	Name string `json:"name,omitempty"`
	// Source is the sandbox it was copied from, which may be gone since.
	Source     string `json:"source"`
	SourceName string `json:"source_name,omitempty"`
	// Digest pins the image the copied layer sits over, so a create refuses a tag that moved.
	Image    string `json:"image"`
	Digest   string `json:"digest"`
	Provider string `json:"provider"`
	// DiskMiB is the bound the source ran under, which a microVM disk copy keeps.
	DiskMiB int64 `json:"disk_mib"`
	// MemoryMiB is the source's memory bound, which a microVM provider needs and a create may change.
	MemoryMiB int64 `json:"memory_mib"`
	// Size is the bytes the copy holds on the host.
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}
