package vfs

import (
	"io/fs"
	"os"
	"time"
)

// Directory exposes a host directory read-write through os.Root, which confines
// symlink and parent traversal to that directory. The caller owns its lifetime.
// As with os.Root, special files and pre-existing hard links are not isolated.
type Directory struct{ root *os.Root }

func OpenDirectory(name string) (*Directory, error) {
	r, err := os.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	return &Directory{root: r}, nil
}
func (d *Directory) Open(name string) (fs.File, error) {
	if !valid(name) {
		return nil, pathErr("open", name, fs.ErrInvalid)
	}
	f, err := d.root.Open(name)
	if err != nil {
		return nil, err
	}
	return &directoryFile{f}, nil
}
func (d *Directory) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	if !valid(name) {
		return nil, pathErr("open", name, fs.ErrInvalid)
	}
	f, err := d.root.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &directoryFile{f}, nil
}
func (d *Directory) Stat(name string) (fs.FileInfo, error) {
	if !valid(name) {
		return nil, pathErr("stat", name, fs.ErrInvalid)
	}
	return d.root.Stat(name)
}
func (d *Directory) Lstat(name string) (fs.FileInfo, error) {
	if !valid(name) {
		return nil, pathErr("lstat", name, fs.ErrInvalid)
	}
	return d.root.Lstat(name)
}
func (d *Directory) ReadDir(name string) ([]fs.DirEntry, error) {
	if !valid(name) {
		return nil, pathErr("readdir", name, fs.ErrInvalid)
	}
	return fs.ReadDir(d.root.FS(), name)
}
func (d *Directory) Mkdir(name string, perm fs.FileMode) error {
	if !valid(name) {
		return pathErr("mkdir", name, fs.ErrInvalid)
	}
	return d.root.Mkdir(name, perm)
}
func (d *Directory) Remove(name string) error {
	if !valid(name) || name == "." {
		return pathErr("remove", name, fs.ErrInvalid)
	}
	return d.root.Remove(name)
}
func (d *Directory) Rename(a, b string) error {
	if !valid(a) || !valid(b) || a == "." || b == "." {
		return pathErr("rename", a, fs.ErrInvalid)
	}
	return d.root.Rename(a, b)
}
func (d *Directory) Readlink(name string) (string, error) {
	if !valid(name) {
		return "", pathErr("readlink", name, fs.ErrInvalid)
	}
	return d.root.Readlink(name)
}
func (d *Directory) Close() error { return d.root.Close() }

func (d *Directory) Chmod(name string, perm fs.FileMode) error {
	if !valid(name) {
		return pathErr("chmod", name, fs.ErrInvalid)
	}
	return d.root.Chmod(name, perm)
}
func (d *Directory) Chtimes(name string, atime, mtime time.Time) error {
	if !valid(name) {
		return pathErr("chtimes", name, fs.ErrInvalid)
	}
	return d.root.Chtimes(name, atime, mtime)
}

var _ WriteFS = (*Directory)(nil)

type directoryFile struct{ *os.File }

func (f *directoryFile) Chtimes(a, m time.Time) error { return fileChtimes(f.File, a, m) }
