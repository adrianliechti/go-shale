// Package vfs provides writable filesystems for the virtual shell.
// Paths within a filesystem follow io/fs: slash-separated, relative, and without '..'.
package vfs

import (
	"io"
	"io/fs"
)

// File is a writable file. Seek, ReadAt, WriteAt, ReadDir, and Truncate are
// optional capabilities used by commands when present.
type File interface {
	fs.File
	io.Writer
}

// WriteFS extends the standard read-only fs.FS contract with mutation operations.
// OpenFile accepts the flags from os.OpenFile. Remove removes one file or empty
// directory. Implementations must confine all paths (including symlinks) to their
// filesystem and support concurrent operations from pipeline stages.
type WriteFS interface {
	fs.FS
	OpenFile(name string, flag int, perm fs.FileMode) (File, error)
	Mkdir(name string, perm fs.FileMode) error
	Remove(name string) error
	Rename(oldName, newName string) error
}
