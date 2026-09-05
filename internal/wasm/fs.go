package wasm

import (
	"bytes"
	"io"
	"io/fs"
	"sort"
	"time"
)

// FS exposes every embedded executable under each of its command names, without
// duplicating bytes. The shell only dispatches these registered paths, not
// arbitrary WASM.
func FS() fs.FS { return commandFS{Commands()} }

type commandFS struct{ names []string }

func (f commandFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return &commandFile{info: commandInfo{name: name}, names: f.names}, nil
	}
	i := sort.SearchStrings(f.names, name)
	if i == len(f.names) || f.names[i] != name {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	m := Lookup(name)
	return &commandFile{info: commandInfo{name, m}, Reader: bytes.NewReader(m.Wasm)}, nil
}

type commandInfo struct {
	name   string
	module *Module
}

func (i commandInfo) Name() string { return i.name }
func (i commandInfo) Size() int64 {
	if i.IsDir() {
		return 0
	}
	return int64(len(i.module.Wasm))
}
func (i commandInfo) Mode() fs.FileMode {
	if i.IsDir() {
		return fs.ModeDir | 0555
	}
	return 0555
}
func (i commandInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i commandInfo) IsDir() bool        { return i.name == "." }
func (i commandInfo) Sys() any           { return nil }

type commandFile struct {
	*bytes.Reader
	info   commandInfo
	names  []string
	offset int
	closed bool
}

func (f *commandFile) Stat() (fs.FileInfo, error) {
	if f.closed {
		return nil, fs.ErrClosed
	}
	return f.info, nil
}
func (f *commandFile) Close() error {
	if f.closed {
		return fs.ErrClosed
	}
	f.closed = true
	return nil
}
func (f *commandFile) Read(p []byte) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if f.info.IsDir() {
		return 0, fs.ErrInvalid
	}
	return f.Reader.Read(p)
}
func (f *commandFile) ReadAt(p []byte, off int64) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if f.info.IsDir() {
		return 0, fs.ErrInvalid
	}
	return f.Reader.ReadAt(p, off)
}
func (f *commandFile) Seek(off int64, whence int) (int64, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if f.info.IsDir() {
		if off != 0 || whence != io.SeekStart {
			return 0, fs.ErrInvalid
		}
		f.offset = 0
		return 0, nil
	}
	return f.Reader.Seek(off, whence)
}
func (f *commandFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if f.closed {
		return nil, fs.ErrClosed
	}
	if !f.info.IsDir() {
		return nil, fs.ErrInvalid
	}
	if n > 0 && f.offset == len(f.names) {
		return nil, io.EOF
	}
	end := len(f.names)
	if n > 0 && n < end-f.offset {
		end = f.offset + n
	}
	entries := make([]fs.DirEntry, 0, end-f.offset)
	for _, name := range f.names[f.offset:end] {
		entries = append(entries, fs.FileInfoToDirEntry(commandInfo{name, Lookup(name)}))
	}
	f.offset = end
	return entries, nil
}
