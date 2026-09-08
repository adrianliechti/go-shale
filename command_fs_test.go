package shale

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/adrianliechti/shale/internal/fsys"
	"github.com/adrianliechti/shale/vfs"
)

func TestCommandFSWrites(t *testing.T) {
	hostPath := t.TempDir()
	host, err := vfs.OpenDirectory(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Close() })
	ns, err := fsys.New([]fsys.Mount{{Path: "/host", FS: host}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := commandFS{ns}
	for _, base := range []string{"work", "host"} {
		t.Run(base, func(t *testing.T) {
			dir := base + "/output"
			name := dir + "/file"
			if err := f.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			write := func(flags int, data string) {
				t.Helper()
				file, err := f.OpenFile(name, flags, 0644)
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.WriteString(file, data)
				if err := errors.Join(err, file.Close()); err != nil {
					t.Fatal(err)
				}
			}
			write(os.O_CREATE|os.O_EXCL|os.O_WRONLY, "hello")
			write(os.O_APPEND|os.O_WRONLY, " world")
			if data, err := fs.ReadFile(f, name); err != nil || string(data) != "hello world" {
				t.Fatalf("append: %q, %v", data, err)
			}
			write(os.O_TRUNC|os.O_RDWR, "updated")
			renamed := dir + "/renamed"
			if err := f.Rename(name, renamed); err != nil {
				t.Fatal(err)
			}
			if _, err := fs.Stat(f, name); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("old name after rename: %v", err)
			}
			if data, err := fs.ReadFile(f, renamed); err != nil || string(data) != "updated" {
				t.Fatalf("truncate and rename: %q, %v", data, err)
			}
			if base == "host" {
				if data, err := os.ReadFile(filepath.Join(hostPath, "output", "renamed")); err != nil || string(data) != "updated" {
					t.Fatalf("host file: %q, %v", data, err)
				}
			}
			if err := f.Remove(dir); err == nil {
				t.Fatal("removed nonempty directory")
			}
			if err := f.Remove(renamed); err != nil {
				t.Fatal(err)
			}
			if err := f.Remove(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := fs.Stat(f, dir); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("removed directory: %v", err)
			}
		})
	}

	// Removing a symlink to a directory must remove the link, not its target.
	if err := host.Mkdir("target", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(hostPath, "link")); err != nil {
		t.Fatal(err)
	}
	if err := f.Remove("host/link"); err != nil {
		t.Fatal(err)
	}
	if info, err := fs.Stat(host, "target"); err != nil || !info.IsDir() {
		t.Fatalf("symlink target: %v, %v", info, err)
	}
}

func TestCommandFSWriteBoundaries(t *testing.T) {
	locked := vfs.NewMemory(0)
	file, err := locked.OpenFile("file", os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(file, "original")
	if err := errors.Join(err, file.Close()); err != nil {
		t.Fatal(err)
	}
	plain := fstest.MapFS{"file": {Data: []byte("original")}}
	ns, err := fsys.New([]fsys.Mount{
		{Path: "/plain", FS: plain},
		{Path: "/locked", FS: locked, ReadOnly: true},
		{Path: "/bin", FS: plain, ReadOnly: true},
		{Path: "/usr/bin", FS: plain, ReadOnly: true},
		{Path: "/writable", FS: vfs.NewMemory(0)},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := commandFS{ns}
	open := func(name string, flags int) error {
		file, err := f.OpenFile(name, flags, 0644)
		if file != nil {
			file.Close()
		}
		return err
	}
	if err := open("work/file", os.O_CREATE|os.O_WRONLY); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"plain", "locked", "bin", "usr/bin"} {
		t.Run(base, func(t *testing.T) {
			name := base + "/file"
			file, err := f.OpenFile(name, os.O_RDONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if data, err := io.ReadAll(file); err != nil || string(data) != "original" {
				t.Fatalf("read-only open: %q, %v", data, err)
			}
			if _, err := file.Write([]byte("changed")); err == nil {
				t.Error("write succeeded on read-only handle")
			}
			for op, err := range map[string]error{
				"write":      open(name, os.O_WRONLY),
				"read-write": open(name, os.O_RDWR),
				"truncate":   open(name, os.O_WRONLY|os.O_TRUNC),
				"append":     open(name, os.O_WRONLY|os.O_APPEND),
				"create":     open(base+"/new", os.O_WRONLY|os.O_CREATE),
				"mkdir":      f.Mkdir(base+"/dir", 0755),
				"remove":     f.Remove(name),
				"rename out": f.Rename(name, "work/moved"),
				"rename in":  f.Rename("work/file", name),
			} {
				if !errors.Is(err, syscall.EROFS) {
					t.Errorf("%s: %v; want EROFS", op, err)
				}
			}
			if data, err := fs.ReadFile(f, name); err != nil || string(data) != "original" {
				t.Fatalf("read-only file changed: %q, %v", data, err)
			}
		})
	}
	for _, name := range []string{"", "/work/file", "../work/file", "work/../file", "work//file", "work/file/", "work/\x00file"} {
		for op, err := range map[string]error{
			"open":       open(name, os.O_CREATE|os.O_WRONLY),
			"mkdir":      f.Mkdir(name, 0755),
			"remove":     f.Remove(name),
			"rename out": f.Rename(name, "work/moved"),
			"rename in":  f.Rename("work/file", name),
		} {
			if !errors.Is(err, fs.ErrInvalid) {
				t.Errorf("%s %q: %v; want ErrInvalid", op, name, err)
			}
		}
	}
	for _, name := range []string{".", "bin", "usr", "usr/bin", "plain", "locked", "writable"} {
		for op, err := range map[string]error{
			"mkdir":      f.Mkdir(name, 0755),
			"remove":     f.Remove(name),
			"rename out": f.Rename(name, "work/moved"),
			"rename in":  f.Rename("work/file", name),
		} {
			if !errors.Is(err, syscall.EBUSY) {
				t.Errorf("%s mount root or ancestor %q: %v; want EBUSY", op, name, err)
			}
		}
	}
	if err := f.Rename("work/file", "writable/file"); !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("cross-mount rename: %v; want EXDEV", err)
	}
	if _, err := fs.Stat(f, "work/file"); err != nil {
		t.Fatalf("source lost after rejected mutations: %v", err)
	}
}
