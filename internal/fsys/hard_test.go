package fsys

import (
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/adrianliechti/shale/vfs"
	ws "github.com/tetratelabs/wazero/experimental/sys"
)

func TestFailedDirectoryOpenDoesNotTruncate(t *testing.T) {
	n, err := New(nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	f, err := n.Open("/work/file", os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.(vfs.File).Write([]byte("keep"))
	f.Close()
	w := &WASI{NS: n, Cwd: "/"}
	if f, errno := w.OpenFile("/work/file", ws.O_DIRECTORY|ws.O_WRONLY|ws.O_TRUNC, 0); errno == 0 {
		f.Close()
		t.Fatal("opened a regular file as directory")
	}
	f, err = n.Open("/work/file", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	i, _ := f.Stat()
	if i.Size() != 4 {
		t.Fatal("failed open destroyed data")
	}
}

func TestDescriptorTimesFollowInodeNotPath(t *testing.T) {
	for _, backend := range []string{"memory", "directory"} {
		t.Run(backend, func(t *testing.T) {
			var storage vfs.WriteFS = vfs.NewMemory(1024)
			if backend == "directory" {
				d, err := vfs.OpenDirectory(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				storage = d
			}
			n, err := New([]Mount{{Path: "/data", FS: storage}}, 1024)
			if err != nil {
				t.Fatal(err)
			}
			w := &WASI{NS: n, Cwd: "/"}
			f, errno := w.OpenFile("/data/file", ws.O_RDWR|ws.O_CREAT, 0644)
			if errno != 0 {
				t.Fatal(errno)
			}
			defer f.Close()
			if err := n.Rename("/data/file", "/data/moved"); err != nil {
				t.Fatal(err)
			}
			g, errno := w.OpenFile("/data/file", ws.O_RDWR|ws.O_CREAT, 0644)
			if errno != 0 {
				t.Fatal(errno)
			}
			g.Close()
			atime, mtime := time.Unix(123456, 0).UnixNano(), time.Unix(234567, 0).UnixNano()
			if errno := f.Utimens(atime, mtime); errno != 0 {
				t.Fatal(errno)
			}
			moved, _ := n.Stat("/data/moved")
			replaced, _ := n.Stat("/data/file")
			if moved.ModTime().UnixNano() != mtime || replaced.ModTime().UnixNano() == mtime {
				t.Fatalf("fd timestamp followed stale pathname: moved %v, replacement %v", moved.ModTime(), replaced.ModTime())
			}
		})
	}
}

func TestReadOnlyDescriptorCannotChangeTimes(t *testing.T) {
	m := vfs.NewMemory(1024)
	f, _ := m.OpenFile("file", os.O_CREATE|os.O_WRONLY, 0644)
	f.Close()
	n, err := New([]Mount{{Path: "/data", FS: m, ReadOnly: true}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	w := &WASI{NS: n, Cwd: "/"}
	fd, errno := w.OpenFile("/data/file", 0, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer fd.Close()
	if errno := fd.Utimens(0, 0); errno != ws.EROFS {
		t.Fatalf("read-only metadata mutation: %v", errno)
	}
	i, _ := fs.Stat(m, "file")
	if i.ModTime().UnixNano() == 0 {
		t.Fatal("read-only mtime changed")
	}
}

func TestUtimeOmitPreservesAccessTime(t *testing.T) {
	n, err := New(nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	w := &WASI{NS: n, Cwd: "/"}
	f, e := w.OpenFile("/work/file", ws.O_RDWR|ws.O_CREAT, 0644)
	if e != 0 {
		t.Fatal(e)
	}
	defer f.Close()
	if e := f.Utimens(1000, 2000); e != 0 {
		t.Fatal(e)
	}
	if e := f.Utimens(ws.UTIME_OMIT, 3000); e != 0 {
		t.Fatal(e)
	}
	i, e := f.Stat()
	if e != 0 || i.Atim != 1000 || i.Mtim != 3000 {
		t.Fatalf("UTIME_OMIT changed atime: %#v, %v", i, e)
	}
}
