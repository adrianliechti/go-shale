package vfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
)

func put(t *testing.T, m *Memory, name, value string) {
	t.Helper()
	f, err := m.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(f, value); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryFS(t *testing.T) {
	m := NewMemory(1024)
	if err := m.Mkdir("dir", 0755); err != nil {
		t.Fatal(err)
	}
	put(t, m, "a", "hello")
	put(t, m, "dir/b", "world")
	if err := fstest.TestFS(m, "a", "dir/b"); err != nil {
		t.Fatal(err)
	}
	if err := m.Rename("dir", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := fstest.TestFS(m, "a", "renamed/b"); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("renamed"); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("nonempty directory: %v", err)
	}
	for _, p := range []string{"", "/", "../escape", "a/../b", "dir//b", "bad\x00name"} {
		if _, err := m.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0644); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("invalid path %q: %v", p, err)
		}
	}
}

func TestMemoryQuotaAndOpenUnlink(t *testing.T) {
	m := NewMemory(8)
	put(t, m, "a", "12345678")
	f, err := m.OpenFile("a", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("a"); err != nil {
		t.Fatal(err)
	}
	g, err := m.OpenFile("b", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := g.Write([]byte("x")); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("unlinked open file lost accounting: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.(io.Seeker).Seek(7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Write(nil); err != nil {
		t.Fatal(err)
	}
	i, _ := g.Stat()
	if i.Size() != 1 {
		t.Fatal("empty write extended file")
	}
	if _, err := g.Write([]byte("z")); err != nil {
		t.Fatal(err)
	}
	b, _ := fs.ReadFile(m, "b")
	if string(b) != "x\x00\x00\x00\x00\x00\x00z" {
		t.Fatalf("sparse write: %q", b)
	}
	if err := g.(interface{ Truncate(int64) error }).Truncate(2); err != nil {
		t.Fatal(err)
	}
	put(t, m, "c", "123456")
}

func TestMemoryConcurrentAppend(t *testing.T) {
	m := NewMemory(1024)
	put(t, m, "a", "")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			f, err := m.OpenFile("a", os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			for range 100 {
				if _, err := f.Write([]byte("x")); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	b, err := fs.ReadFile(m, "a")
	if err != nil || len(b) != 800 {
		t.Fatalf("append: %d bytes, %v", len(b), err)
	}
}
