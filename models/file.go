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
	Type  FileType  `json:"type" enum:"file,dir,symlink,other"`
	Size  int64     `json:"size" doc:"The logical size in bytes."`
	Mode  uint32    `json:"mode" doc:"The permission bits with setuid, setgid and sticky as a number, at most 0o7777 (4095); the type is in type."`
	UID   uint32    `json:"uid"`
	GID   uint32    `json:"gid"`
	MTime time.Time `json:"mtime"`
}

// FileEntry is one name in a guest directory with its own stat, never what a symlink points to.
type FileEntry struct {
	Name string `json:"name"`
	FileStat
}
