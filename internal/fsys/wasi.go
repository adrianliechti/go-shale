package fsys

import (
	"errors"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"

	ws "github.com/tetratelabs/wazero/experimental/sys"
	"github.com/tetratelabs/wazero/sys"
)

// WASI is the sole adapter between WASI's filesystem calls and our namespace.
// The working directory is per command; no process-wide chdir is involved.
type WASI struct {
	ws.UnimplementedFS
	NS  *Namespace
	Cwd string
}

func (w *WASI) name(p string) string { return Resolve(w.Cwd, p) }
func (w *WASI) OpenFile(p string, flag ws.Oflag, perm fs.FileMode) (ws.File, ws.Errno) {
	// Validate before a mutating open: O_DIRECTORY|O_TRUNC must never truncate
	// a regular file and only then discover that it is not a directory.
	if flag&ws.O_DIRECTORY != 0 {
		i, e := w.NS.Stat(w.name(p))
		if e != nil {
			return nil, errno(e)
		}
		if !i.IsDir() {
			return nil, ws.ENOTDIR
		}
	}
	if flag&ws.O_NOFOLLOW != 0 {
		if i, e := w.NS.Lstat(w.name(p)); e == nil && i.Mode()&fs.ModeSymlink != 0 {
			return nil, ws.ELOOP
		}
	}
	of := os.O_RDONLY
	if flag&ws.O_RDWR != 0 {
		of = os.O_RDWR
	} else if flag&ws.O_WRONLY != 0 {
		of = os.O_WRONLY
	}
	for _, pair := range []struct {
		w ws.Oflag
		g int
	}{{ws.O_APPEND, os.O_APPEND}, {ws.O_CREAT, os.O_CREATE}, {ws.O_EXCL, os.O_EXCL}, {ws.O_TRUNC, os.O_TRUNC}} {
		if flag&pair.w != 0 {
			of |= pair.g
		}
	}
	f, err := w.NS.Open(w.name(p), of, perm)
	if err != nil {
		return nil, errno(err)
	}
	if flag&ws.O_DIRECTORY != 0 {
		info, e := f.Stat()
		if e != nil || !info.IsDir() {
			f.Close()
			if e != nil {
				return nil, errno(e)
			}
			return nil, ws.ENOTDIR
		}
	}
	_, _, metadataErr := w.NS.writable(w.name(p))
	return &wasiFile{f: f, ns: w.NS, metadataWritable: metadataErr == nil, name: w.name(p), readable: of&os.O_WRONLY == 0, writable: of&(os.O_WRONLY|os.O_RDWR) != 0, appendMode: of&os.O_APPEND != 0}, 0
}
func (w *WASI) Stat(p string) (sys.Stat_t, ws.Errno) {
	i, e := w.NS.Stat(w.name(p))
	if e != nil {
		return sys.Stat_t{}, errno(e)
	}
	return stat(i, w.name(p)), 0
}
func (w *WASI) Lstat(p string) (sys.Stat_t, ws.Errno) {
	i, e := w.NS.Lstat(w.name(p))
	if e != nil {
		return sys.Stat_t{}, errno(e)
	}
	return stat(i, w.name(p)), 0
}
func (w *WASI) Readlink(p string) (string, ws.Errno) {
	v, e := w.NS.Readlink(w.name(p))
	return v, errno(e)
}
func (w *WASI) Mkdir(p string, perm fs.FileMode) ws.Errno { return errno(w.NS.Mkdir(w.name(p), perm)) }
func (w *WASI) Unlink(p string) ws.Errno                  { return errno(w.NS.Remove(w.name(p), false)) }
func (w *WASI) Rmdir(p string) ws.Errno                   { return errno(w.NS.Remove(w.name(p), true)) }
func (w *WASI) Rename(a, b string) ws.Errno               { return errno(w.NS.Rename(w.name(a), w.name(b))) }
func (w *WASI) Chmod(p string, perm fs.FileMode) ws.Errno { return errno(w.NS.Chmod(w.name(p), perm)) }
func (w *WASI) Utimens(p string, a, m int64) ws.Errno {
	i, e := w.NS.Stat(w.name(p))
	if e != nil {
		return errno(e)
	}
	at, mt := timestamps(stat(i, w.name(p)), a, m)
	return errno(w.NS.Chtimes(w.name(p), at, mt))
}

func timestamps(st sys.Stat_t, a, m int64) (time.Time, time.Time) {
	if a == ws.UTIME_OMIT {
		a = st.Atim
	}
	if m == ws.UTIME_OMIT {
		m = st.Mtim
	}
	return time.Unix(0, a), time.Unix(0, m)
}

func stat(i fs.FileInfo, name string) sys.Stat_t {
	s := sys.NewStat_t(i)
	if at, ok := i.(interface{ AccessTime() time.Time }); ok {
		s.Atim = at.AccessTime().UnixNano()
	}
	// Preserve host identity, including hard-link aliases used by cp's same-file
	// checks. Memory nodes have process-unique IDs on a separate virtual device.
	if ino, ok := i.(interface{ Inode() uint64 }); ok {
		s.Dev = ^uint64(0)
		s.Ino = ino.Inode()
		return s
	}
	if s.Ino != 0 {
		return s
	}
	s.Dev = ^uint64(0) - 1
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	s.Ino = sys.Inode(h.Sum64())
	return s
}

func errno(e error) ws.Errno {
	if e == nil || errors.Is(e, io.EOF) {
		return 0
	}
	for _, p := range []struct {
		e error
		n ws.Errno
	}{{syscall.EROFS, ws.EROFS}, {syscall.ENOTDIR, ws.ENOTDIR}, {syscall.EISDIR, ws.EISDIR}, {syscall.ENOTEMPTY, ws.ENOTEMPTY}, {syscall.ENOSPC, ws.EIO}, {syscall.EBUSY, ws.EPERM}, {syscall.EXDEV, ws.ENOTSUP}, {syscall.ENOSYS, ws.ENOSYS}, {fs.ErrNotExist, ws.ENOENT}, {fs.ErrExist, ws.EEXIST}, {fs.ErrPermission, ws.EACCES}, {fs.ErrClosed, ws.EBADF}, {fs.ErrInvalid, ws.EINVAL}} {
		if errors.Is(e, p.e) {
			return p.n
		}
	}
	return ws.EIO
}

type wasiFile struct {
	ws.UnimplementedFile
	f                                      fs.File
	ns                                     *Namespace
	metadataWritable                       bool
	name                                   string
	readable, writable, appendMode, closed bool
}

func (f *wasiFile) Dev() (uint64, ws.Errno)    { s, e := f.Stat(); return s.Dev, e }
func (f *wasiFile) Ino() (sys.Inode, ws.Errno) { s, e := f.Stat(); return s.Ino, e }
func (f *wasiFile) IsDir() (bool, ws.Errno)    { s, e := f.Stat(); return s.Mode.IsDir(), e }
func (f *wasiFile) IsAppend() bool             { return f.appendMode }
func (f *wasiFile) Stat() (sys.Stat_t, ws.Errno) {
	if f.closed {
		return sys.Stat_t{}, ws.EBADF
	}
	i, e := f.f.Stat()
	if e != nil {
		return sys.Stat_t{}, errno(e)
	}
	return stat(i, f.name), 0
}
func (f *wasiFile) Read(p []byte) (int, ws.Errno) {
	if f.closed || !f.readable {
		return 0, ws.EBADF
	}
	n, e := f.f.Read(p)
	return n, errno(e)
}
func (f *wasiFile) Pread(p []byte, off int64) (int, ws.Errno) {
	if f.closed || !f.readable {
		return 0, ws.EBADF
	}
	if r, ok := f.f.(io.ReaderAt); ok {
		n, e := r.ReadAt(p, off)
		return n, errno(e)
	}
	return 0, ws.ENOSYS
}

// Seek implements wazero's syscall interface, deliberately not io.Seeker.
// go vet's stdmethods analyzer assumes an error result for this name; run vet
// with -stdmethods=false for this adapter (see README).
func (f *wasiFile) Seek(off int64, whence int) (int64, ws.Errno) {
	if f.closed {
		return 0, ws.EBADF
	}
	if s, ok := f.f.(io.Seeker); ok {
		n, e := s.Seek(off, whence)
		return n, errno(e)
	}
	return 0, ws.ENOSYS
}
func (f *wasiFile) Write(p []byte) (int, ws.Errno) {
	if f.closed || !f.writable {
		return 0, ws.EBADF
	}
	if w, ok := f.f.(io.Writer); ok {
		n, e := w.Write(p)
		return n, errno(e)
	}
	return 0, ws.EBADF
}
func (f *wasiFile) Pwrite(p []byte, off int64) (int, ws.Errno) {
	if f.closed || !f.writable {
		return 0, ws.EBADF
	}
	if w, ok := f.f.(io.WriterAt); ok {
		n, e := w.WriteAt(p, off)
		return n, errno(e)
	}
	return 0, ws.ENOSYS
}
func (f *wasiFile) Truncate(size int64) ws.Errno {
	if f.closed || !f.writable {
		return ws.EBADF
	}
	if t, ok := f.f.(interface{ Truncate(int64) error }); ok {
		return errno(t.Truncate(size))
	}
	return ws.ENOSYS
}
func (f *wasiFile) Utimens(a, m int64) ws.Errno {
	if f.closed {
		return ws.EBADF
	}
	if !f.metadataWritable {
		return ws.EROFS
	}
	st, e := f.Stat()
	if e != 0 {
		return e
	}
	at, mt := timestamps(st, a, m)
	if c, ok := f.f.(interface {
		Chtimes(time.Time, time.Time) error
	}); ok {
		return errno(c.Chtimes(at, mt))
	}
	// Never fall back to the original pathname: it may now name another inode.
	return ws.ENOSYS
}
func (f *wasiFile) Sync() ws.Errno {
	if f.closed {
		return ws.EBADF
	}
	if s, ok := f.f.(interface{ Sync() error }); ok {
		return errno(s.Sync())
	}
	return 0
}
func (f *wasiFile) Datasync() ws.Errno { return f.Sync() }
func (f *wasiFile) Readdir(n int) ([]ws.Dirent, ws.Errno) {
	if f.closed {
		return nil, ws.EBADF
	}
	r, ok := f.f.(fs.ReadDirFile)
	if !ok {
		return nil, ws.ENOTDIR
	}
	es, e := r.ReadDir(n)
	if e != nil && !errors.Is(e, io.EOF) {
		return nil, errno(e)
	}
	result := make([]ws.Dirent, 0, len(es))
	for _, entry := range es {
		result = append(result, ws.Dirent{Name: entry.Name(), Type: entry.Type()})
	}
	return result, 0
}
func (f *wasiFile) Close() ws.Errno {
	if f.closed {
		return ws.EBADF
	}
	f.closed = true
	return errno(f.f.Close())
}

var _ ws.FS = (*WASI)(nil)
