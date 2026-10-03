package models

import "time"

// FileType is what one guest path is, in the words the stat header carries.
type FileType string

const (
	FileRegular FileType = "file"
	FileDir     FileType = "dir"
	FileSymlink FileType = "symlink"
	FileOther   FileType = "other"
)

// FileStat is one guest path as the guest sees it: the X-Shard-Stat header of the file API, as JSON.
type FileStat struct {
	Type FileType `json:"type"`
	Size int64    `json:"size"`
	// Mode is the permission bits with setuid, setgid and sticky, at most 0o7777; the type is in Type.
	Mode  uint32    `json:"mode"`
	UID   uint32    `json:"uid"`
	GID   uint32    `json:"gid"`
	MTime time.Time `json:"mtime"`
}

// FileEntry is one name in a guest directory with its own stat, never what a symlink points to.
type FileEntry struct {
	Name string `json:"name"`
	FileStat
}
