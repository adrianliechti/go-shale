package vfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Memory is a concurrent writable in-memory filesystem. Its byte budget covers
// file contents, including unlinked files while they remain open.
type Memory struct {
	mu          sync.Mutex
	nodes       map[string]*node
	limit, used int64
}

var nextInode atomic.Uint64

type node struct {
	data   []byte
	mode   fs.FileMode
	mtime  time.Time
	atime  time.Time
	id     uint64
	refs   int
	linked bool
}

// NewMemory creates an empty filesystem. A nonpositive limit defaults to 64 MiB.
func NewMemory(maxBytes int64) *Memory {
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	m := &Memory{nodes: make(map[string]*node), limit: maxBytes}
	m.nodes["."] = m.newNode(fs.ModeDir | 0755)
	return m
}

func (m *Memory) newNode(mode fs.FileMode) *node {
	now := time.Now()
	return &node{mode: mode, mtime: now, atime: now, id: nextInode.Add(1), linked: true}
}

func valid(name string) bool                   { return fs.ValidPath(name) && !strings.ContainsRune(name, 0) }
func pathErr(op, name string, err error) error { return &fs.PathError{Op: op, Path: name, Err: err} }

func (m *Memory) lookup(name string) (*node, error) {
	if !valid(name) {
		return nil, fs.ErrInvalid
	}
	n, ok := m.nodes[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return n, nil
}

func (m *Memory) parent(name string) error {
	n, err := m.lookup(path.Dir(name))
	if err != nil {
		return err
	}
	if !n.mode.IsDir() {
		return syscall.ENOTDIR
	}
	return nil
}

func (m *Memory) Open(name string) (fs.File, error) { return m.OpenFile(name, os.O_RDONLY, 0) }

func (m *Memory) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !valid(name) {
		return nil, pathErr("open", name, fs.ErrInvalid)
	}
	n, err := m.lookup(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) || flag&os.O_CREATE == 0 {
			return nil, pathErr("open", name, err)
		}
		if err = m.parent(name); err != nil {
			return nil, pathErr("open", name, err)
		}
		if len(m.nodes) >= 10000 {
			return nil, pathErr("open", name, syscall.ENOSPC)
		}
		n = m.newNode(perm.Perm())
		m.nodes[name] = n
	} else if flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0 {
		return nil, pathErr("open", name, fs.ErrExist)
	}
	writable := flag&(os.O_WRONLY|os.O_RDWR) != 0
	if n.mode.IsDir() && (writable || flag&os.O_TRUNC != 0) {
		return nil, pathErr("open", name, syscall.EISDIR)
	}
	if flag&os.O_TRUNC != 0 {
		if !writable {
			return nil, pathErr("open", name, fs.ErrPermission)
		}
		m.used -= int64(len(n.data))
		n.data = nil
		n.mtime = time.Now()
	}
	n.refs++
	return &memoryFile{m: m, n: n, name: name, readable: flag&os.O_WRONLY == 0, writable: writable, appendMode: flag&os.O_APPEND != 0}, nil
}

func (m *Memory) Stat(name string) (fs.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.lookup(name)
	if err != nil {
		return nil, pathErr("stat", name, err)
	}
	return infoFor(name, n), nil
}

func (m *Memory) ReadDir(name string) ([]fs.DirEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readDir(name)
}

func (m *Memory) readDir(name string) ([]fs.DirEntry, error) {
	n, err := m.lookup(name)
	if err != nil {
		return nil, pathErr("readdir", name, err)
	}
	if !n.mode.IsDir() {
		return nil, pathErr("readdir", name, syscall.ENOTDIR)
	}
	entries := []fs.DirEntry{}
	for p, child := range m.nodes {
		if p != name && path.Dir(p) == name {
			entries = append(entries, fs.FileInfoToDirEntry(infoFor(p, child)))
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func (m *Memory) Mkdir(name string, perm fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !valid(name) {
		return pathErr("mkdir", name, fs.ErrInvalid)
	}
	if _, ok := m.nodes[name]; ok {
		return pathErr("mkdir", name, fs.ErrExist)
	}
	if err := m.parent(name); err != nil {
		return pathErr("mkdir", name, err)
	}
	if len(m.nodes) >= 10000 {
		return pathErr("mkdir", name, syscall.ENOSPC)
	}
	m.nodes[name] = m.newNode(fs.ModeDir | perm.Perm())
	return nil
}

func (m *Memory) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.lookup(name)
	if err != nil {
		return pathErr("remove", name, err)
	}
	if name == "." {
		return pathErr("remove", name, fs.ErrPermission)
	}
	if n.mode.IsDir() {
		for p := range m.nodes {
			if path.Dir(p) == name {
				return pathErr("remove", name, syscall.ENOTEMPTY)
			}
		}
	}
	delete(m.nodes, name)
	m.unlink(n)
	return nil
}

func (m *Memory) unlink(n *node) {
	n.linked = false
	if n.refs == 0 {
		m.used -= int64(len(n.data))
		n.data = nil
	}
}

func (m *Memory) Rename(oldName, newName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.lookup(oldName)
	if err != nil {
		return pathErr("rename", oldName, err)
	}
	if !valid(newName) {
		return pathErr("rename", newName, fs.ErrInvalid)
	}
	if oldName == "." || newName == "." {
		return pathErr("rename", oldName, fs.ErrPermission)
	}
	if oldName == newName {
		return nil
	}
	if strings.HasPrefix(newName, oldName+"/") {
		return pathErr("rename", newName, fs.ErrInvalid)
	}
	if err := m.parent(newName); err != nil {
		return pathErr("rename", newName, err)
	}
	if dst := m.nodes[newName]; dst != nil {
		if n.mode.IsDir() && !dst.mode.IsDir() {
			return pathErr("rename", newName, syscall.ENOTDIR)
		}
		if !n.mode.IsDir() && dst.mode.IsDir() {
			return pathErr("rename", newName, syscall.EISDIR)
		}
		if dst.mode.IsDir() {
			for p := range m.nodes {
				if path.Dir(p) == newName {
					return pathErr("rename", newName, syscall.ENOTEMPTY)
				}
			}
		}
		m.unlink(dst)
	}
	moved := make(map[string]*node)
	for p, child := range m.nodes {
		if p == oldName || strings.HasPrefix(p, oldName+"/") {
			moved[newName+strings.TrimPrefix(p, oldName)] = child
			delete(m.nodes, p)
		}
	}
	for p, child := range moved {
		m.nodes[p] = child
	}
	return nil
}

func (m *Memory) Chmod(name string, perm fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.lookup(name)
	if err != nil {
		return pathErr("chmod", name, err)
	}
	n.mode = n.mode.Type() | perm.Perm()
	return nil
}

func (m *Memory) Chtimes(name string, atime, mtime time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.lookup(name)
	if err != nil {
		return pathErr("chtimes", name, err)
	}
	n.atime, n.mtime = atime, mtime
	return nil
}

type fileInfo struct {
	name  string
	size  int64
	mode  fs.FileMode
	mtime time.Time
	atime time.Time
	id    uint64
}

func infoFor(name string, n *node) fileInfo {
	return fileInfo{path.Base(name), int64(len(n.data)), n.mode, n.mtime, n.atime, n.id}
}
func (i fileInfo) Name() string          { return i.name }
func (i fileInfo) Size() int64           { return i.size }
func (i fileInfo) Mode() fs.FileMode     { return i.mode }
func (i fileInfo) ModTime() time.Time    { return i.mtime }
func (i fileInfo) IsDir() bool           { return i.mode.IsDir() }
func (i fileInfo) Sys() any              { return nil }
func (i fileInfo) Inode() uint64         { return i.id }
func (i fileInfo) AccessTime() time.Time { return i.atime }

type memoryFile struct {
	m                                      *Memory
	n                                      *node
	name                                   string
	pos                                    int64
	readable, writable, appendMode, closed bool
	entries                                []fs.DirEntry
	dirPos                                 int
}

func (f *memoryFile) Stat() (fs.FileInfo, error) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.closed {
		return nil, fs.ErrClosed
	}
	return infoFor(f.name, f.n), nil
}
func (f *memoryFile) Close() error {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.closed {
		return fs.ErrClosed
	}
	f.closed = true
	f.n.refs--
	if !f.n.linked && f.n.refs == 0 {
		f.m.used -= int64(len(f.n.data))
		f.n.data = nil
	}
	return nil
}
func (f *memoryFile) readAt(p []byte, off int64) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if !f.readable {
		return 0, fs.ErrPermission
	}
	if f.n.mode.IsDir() {
		return 0, syscall.EISDIR
	}
	if off < 0 {
		return 0, fs.ErrInvalid
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= int64(len(f.n.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.n.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (f *memoryFile) Read(p []byte) (int, error) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	n, err := f.readAt(p, f.pos)
	f.pos += int64(n)
	return n, err
}
func (f *memoryFile) ReadAt(p []byte, off int64) (int, error) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	return f.readAt(p, off)
}
func (f *memoryFile) resize(size int64) error {
	if size < 0 {
		return fs.ErrInvalid
	}
	if size > f.m.limit {
		return syscall.ENOSPC
	}
	delta := size - int64(len(f.n.data))
	if delta > f.m.limit-f.m.used {
		return syscall.ENOSPC
	}
	if delta > 0 {
		f.n.data = append(f.n.data, make([]byte, int(delta))...)
	} else if delta < 0 {
		// Release the old allocation when shrinking, so repeated truncation does
		// not retain capacity that is no longer charged to the byte budget.
		f.n.data = append([]byte(nil), f.n.data[:int(size)]...)
	}
	f.m.used += delta
	f.n.mtime = time.Now()
	return nil
}
func (f *memoryFile) writeAt(p []byte, off int64) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if !f.writable {
		return 0, fs.ErrPermission
	}
	if off < 0 {
		return 0, fs.ErrInvalid
	}
	if off > f.m.limit-int64(len(p)) {
		return 0, syscall.ENOSPC
	}
	if len(p) == 0 {
		return 0, nil
	}
	if end := off + int64(len(p)); end > int64(len(f.n.data)) {
		if err := f.resize(end); err != nil {
			return 0, err
		}
	}
	n := copy(f.n.data[off:], p)
	f.n.mtime = time.Now()
	return n, nil
}
func (f *memoryFile) Write(p []byte) (int, error) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.appendMode {
		f.pos = int64(len(f.n.data))
	}
	n, err := f.writeAt(p, f.pos)
	f.pos += int64(n)
	return n, err
}
func (f *memoryFile) WriteAt(p []byte, off int64) (int, error) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	if f.appendMode {
		return 0, fs.ErrInvalid
	}
	return f.writeAt(p, off)
}
func (f *memoryFile) Seek(off int64, whence int) (int64, error) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	base := int64(0)
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = f.pos
	case io.SeekEnd:
		base = int64(len(f.n.data))
	default:
		return 0, fs.ErrInvalid
	}
	if off < -base || off > f.m.limit-base {
		return 0, fs.ErrInvalid
	}
	f.pos = base + off
	if f.pos == 0 {
		f.dirPos = 0
		f.entries = nil
	}
	return f.pos, nil
}
func (f *memoryFile) Truncate(size int64) error {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.closed {
		return fs.ErrClosed
	}
	if !f.writable {
		return fs.ErrPermission
	}
	return f.resize(size)
}
func (f *memoryFile) Chtimes(a, m time.Time) error {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.closed {
		return fs.ErrClosed
	}
	f.n.atime, f.n.mtime = a, m
	return nil
}
func (f *memoryFile) ReadDir(n int) ([]fs.DirEntry, error) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.closed {
		return nil, fs.ErrClosed
	}
	if f.entries == nil {
		// The handle identifies the node, not the name used to open it. That
		// name may have been renamed and reused for an unrelated directory.
		name := ""
		for p, node := range f.m.nodes {
			if node == f.n {
				name = p
				break
			}
		}
		if name == "" {
			return nil, fs.ErrNotExist
		}
		var err error
		f.entries, err = f.m.readDir(name)
		if err != nil {
			return nil, err
		}
	}
	if f.dirPos == len(f.entries) && n > 0 {
		return nil, io.EOF
	}
	end := len(f.entries)
	if n > 0 && n < end-f.dirPos {
		end = f.dirPos + n
	}
	entries := f.entries[f.dirPos:end]
	f.dirPos = end
	return entries, nil
}

var _ WriteFS = (*Memory)(nil)
