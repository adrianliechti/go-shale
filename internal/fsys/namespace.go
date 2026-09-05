package fsys

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/adrianliechti/shale/vfs"
)

type Mount struct {
	Path     string
	FS       fs.FS
	ReadOnly bool
}
type Namespace struct {
	root   *vfs.Memory
	mounts []Mount
}

func New(mounts []Mount, maxBytes int64) (*Namespace, error) {
	n := &Namespace{root: vfs.NewMemory(maxBytes), mounts: append([]Mount(nil), mounts...)}
	for _, dir := range []string{"tmp", "work", "dev"} {
		if err := n.root.Mkdir(dir, 0755); err != nil {
			return nil, err
		}
	}
	for i, m := range mounts {
		if m.FS == nil || !strings.HasPrefix(m.Path, "/") || m.Path == "/" || path.Clean(m.Path) != m.Path || strings.ContainsRune(m.Path, 0) {
			return nil, errors.New("invalid mount path: " + m.Path)
		}
		for _, other := range mounts[:i] {
			if m.Path == other.Path || strings.HasPrefix(m.Path, other.Path+"/") || strings.HasPrefix(other.Path, m.Path+"/") {
				return nil, errors.New("overlapping mounts are not supported")
			}
		}
		info, err := fs.Stat(m.FS, ".")
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, syscall.ENOTDIR
		}
		parts := strings.Split(strings.TrimPrefix(m.Path, "/"), "/")
		for j := range parts {
			err := n.root.Mkdir(strings.Join(parts[:j+1], "/"), 0755)
			if err != nil && !errors.Is(err, fs.ErrExist) {
				return nil, err
			}
		}
	}
	sort.Slice(n.mounts, func(i, j int) bool { return len(n.mounts[i].Path) > len(n.mounts[j].Path) })
	return n, nil
}

// Resolve performs Unix path resolution within the guest namespace only.
func Resolve(cwd, name string) string {
	directory := strings.HasSuffix(name, "/") || path.Base(name) == "."
	if !strings.HasPrefix(name, "/") {
		name = path.Join(cwd, name)
	}
	clean := path.Clean("/" + name)
	if directory && clean != "/" {
		return clean + "/"
	}
	return clean
}

func (n *Namespace) route(name string) (fs.FS, string, bool, error) {
	if strings.ContainsRune(name, 0) {
		return nil, "", false, fs.ErrInvalid
	}
	name = path.Clean(Resolve("/", name))
	for _, m := range n.mounts {
		if name == m.Path || strings.HasPrefix(name, m.Path+"/") {
			rel := strings.TrimPrefix(strings.TrimPrefix(name, m.Path), "/")
			if rel == "" {
				rel = "."
			}
			return m.FS, rel, m.ReadOnly, nil
		}
	}
	rel := strings.TrimPrefix(name, "/")
	if rel == "" {
		rel = "."
	}
	return n.root, rel, false, nil
}

func (n *Namespace) Open(name string, flag int, perm fs.FileMode) (fs.File, error) {
	if strings.HasSuffix(name, "/") {
		if _, err := n.Stat(name); err != nil {
			return nil, err
		}
	}
	if Resolve("/", name) == "/dev/null" {
		return &nullFile{}, nil
	}
	f, rel, ro, err := n.route(name)
	if err != nil {
		return nil, err
	}
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		w, ok := f.(vfs.WriteFS)
		if ro || !ok {
			return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EROFS}
		}
		return w.OpenFile(rel, flag, perm)
	}
	return f.Open(rel)
}
func (n *Namespace) Stat(name string) (fs.FileInfo, error) {
	if Resolve("/", name) == "/dev/null" {
		return nullInfo{}, nil
	}
	if Resolve("/", name) == "/dev/null/" {
		return nil, syscall.ENOTDIR
	}
	f, rel, _, err := n.route(name)
	if err != nil {
		return nil, err
	}
	i, err := fs.Stat(f, rel)
	if err == nil && strings.HasSuffix(name, "/") && !i.IsDir() {
		return nil, syscall.ENOTDIR
	}
	return i, err
}
func (n *Namespace) Lstat(name string) (fs.FileInfo, error) {
	if strings.HasSuffix(name, "/") {
		return n.Stat(name)
	}
	f, rel, _, err := n.route(name)
	if err != nil {
		return nil, err
	}
	if l, ok := f.(interface {
		Lstat(string) (fs.FileInfo, error)
	}); ok {
		return l.Lstat(rel)
	}
	return n.Stat(name)
}
func (n *Namespace) Readlink(name string) (string, error) {
	f, rel, _, err := n.route(name)
	if err != nil {
		return "", err
	}
	if l, ok := f.(interface{ Readlink(string) (string, error) }); ok {
		return l.Readlink(rel)
	}
	return "", syscall.ENOSYS
}
func (n *Namespace) ReadDir(name string) ([]fs.DirEntry, error) {
	f, rel, _, err := n.route(name)
	if err != nil {
		return nil, err
	}
	return fs.ReadDir(f, rel)
}
func (n *Namespace) writable(name string) (vfs.WriteFS, string, error) {
	clean := path.Clean(Resolve("/", name))
	for _, m := range n.mounts {
		if clean == m.Path || strings.HasPrefix(m.Path, clean+"/") || clean == "/" {
			return nil, "", syscall.EBUSY
		}
	}
	f, rel, ro, err := n.route(name)
	if err != nil {
		return nil, "", err
	}
	w, ok := f.(vfs.WriteFS)
	if ro || !ok {
		return nil, "", syscall.EROFS
	}
	return w, rel, nil
}
func (n *Namespace) Mkdir(name string, perm fs.FileMode) error {
	f, rel, err := n.writable(name)
	if err != nil {
		return err
	}
	return f.Mkdir(rel, perm)
}
func (n *Namespace) Remove(name string, dir bool) error {
	f, rel, err := n.writable(name)
	if err != nil {
		return err
	}
	info, err := n.Lstat(name)
	if err != nil {
		return err
	}
	if info.IsDir() != dir {
		if dir {
			return syscall.ENOTDIR
		}
		return syscall.EISDIR
	}
	return f.Remove(rel)
}
func (n *Namespace) Rename(old, new string) error {
	for _, name := range []string{old, new} {
		if strings.HasSuffix(name, "/") {
			if _, err := n.Stat(name); err != nil {
				return err
			}
		}
	}
	f, a, err := n.writable(old)
	if err != nil {
		return err
	}
	g, b, err := n.writable(new)
	if err != nil {
		return err
	}
	// Compare mount ownership by routing paths, not interface equality: custom
	// filesystems are not required to have comparable dynamic types.
	owner := func(p string) string {
		p = path.Clean(Resolve("/", p))
		for _, m := range n.mounts {
			if p == m.Path || strings.HasPrefix(p, m.Path+"/") {
				return m.Path
			}
		}
		return "/"
	}
	if owner(old) != owner(new) {
		return syscall.EXDEV
	}
	_ = g
	return f.Rename(a, b)
}
func (n *Namespace) Chmod(name string, perm fs.FileMode) error {
	f, rel, err := n.writable(name)
	if err != nil {
		return err
	}
	if c, ok := f.(interface {
		Chmod(string, fs.FileMode) error
	}); ok {
		return c.Chmod(rel, perm)
	}
	return syscall.ENOSYS
}
func (n *Namespace) Chtimes(name string, a, m time.Time) error {
	f, rel, err := n.writable(name)
	if err != nil {
		return err
	}
	if c, ok := f.(interface {
		Chtimes(string, time.Time, time.Time) error
	}); ok {
		return c.Chtimes(rel, a, m)
	}
	return syscall.ENOSYS
}

type nullFile struct{}

func (*nullFile) Read([]byte) (int, error)    { return 0, io.EOF }
func (*nullFile) Write(p []byte) (int, error) { return len(p), nil }
func (*nullFile) Close() error                { return nil }
func (*nullFile) Stat() (fs.FileInfo, error)  { return nullInfo{}, nil }

type nullInfo struct{}

func (nullInfo) Name() string       { return "null" }
func (nullInfo) Size() int64        { return 0 }
func (nullInfo) Mode() fs.FileMode  { return fs.ModeDevice | fs.ModeCharDevice | 0666 }
func (nullInfo) ModTime() time.Time { return time.Time{} }
func (nullInfo) IsDir() bool        { return false }
func (nullInfo) Sys() any           { return nil }
