package shale_test

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	shale "github.com/adrianliechti/shale"
	"github.com/adrianliechti/shale/vfs"
)

func TestCanceledOperationsDoNotTouchBackend(t *testing.T) {
	m := &countingFS{Memory: vfs.NewMemory(1024)}
	f, _ := m.Memory.OpenFile("file", os.O_CREATE|os.O_WRONLY, 0644)
	f.Close()
	b := newShell(t, shale.Options{Mounts: []shale.Mount{{Path: "/data", FS: m}}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 100 {
		if _, err := b.ReadFile(ctx, "/data/file"); !errors.Is(err, context.Canceled) {
			t.Fatalf("read with canceled context: %v", err)
		}
	}
	if m.opens != 0 {
		t.Fatalf("canceled reads opened backend %d times", m.opens)
	}
}

type countingFS struct {
	*vfs.Memory
	opens int
}

func (f *countingFS) Open(p string) (fs.File, error) { f.opens++; return f.Memory.Open(p) }

func TestConcurrentCallsSerializeState(t *testing.T) {
	b := newShell(t, shale.Options{Timeout: 5 * time.Second})
	if _, err := b.Exec(t.Context(), "counter=0"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			if _, err := b.Exec(t.Context(), "counter=$((counter+1))"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	r, err := b.Exec(t.Context(), "echo $counter")
	if err != nil || r.Stdout != "40\n" {
		t.Fatalf("lost state update: %#v, %v", r, err)
	}
	if err := b.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(t.Context(), "echo no"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("exec after close: %v", err)
	}
	if _, err := b.ReadFile(t.Context(), "/work/file"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("read after close: %v", err)
	}
}

func TestQueuedDeadlineAndRecovery(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	m := &gatedFS{Memory: vfs.NewMemory(1024), entered: entered, release: release}
	f, _ := m.Memory.OpenFile("file", os.O_CREATE|os.O_WRONLY, 0644)
	f.Write([]byte("done"))
	f.Close()
	b := newShell(t, shale.Options{Mounts: []shale.Mount{{Path: "/data", FS: m}}})
	done := make(chan error, 1)
	go func() { _, err := b.Exec(t.Context(), "cat /data/file"); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("command never entered backend")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := b.Exec(ctx, "echo should-not-run > /work/queued")
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("queued deadline: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadFile(t.Context(), "/work/queued"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expired queued execution ran: %v", err)
	}
}

type gatedFS struct {
	*vfs.Memory
	entered, release chan struct{}
	once             sync.Once
}

func (f *gatedFS) Open(p string) (fs.File, error) {
	if p == "file" {
		f.once.Do(func() { close(f.entered); <-f.release })
	}
	return f.Memory.Open(p)
}

func TestReadFileLimitIntegerBoundary(t *testing.T) {
	b := newShell(t, shale.Options{MaxFileBytes: math.MaxInt64})
	if _, err := b.Exec(t.Context(), "echo contents > file"); err != nil {
		t.Fatal(err)
	}
	data, err := b.ReadFile(t.Context(), "file")
	if err != nil || string(data) != "contents\n" {
		t.Fatalf("integer overflow silently truncated read: %q, %v", data, err)
	}
}

func TestTrailingSlashDoesNotOverwriteRegularFile(t *testing.T) {
	b := newShell(t, shale.Options{})
	if _, err := b.Exec(t.Context(), "echo keep > /work/file"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/work/file/", "/work/file/.", "/work/file//"} {
		r, err := b.Exec(t.Context(), "echo bad > "+p)
		if err != nil || r.ExitCode == 0 {
			t.Fatalf("regular file accepted as directory: %s: %#v, %v", p, r, err)
		}
	}
	data, err := b.ReadFile(t.Context(), "/work/file")
	if err != nil || string(data) != "keep\n" {
		t.Fatalf("failed redirection destroyed data: %q, %v", data, err)
	}
}

func TestRedirectionCloseFailureIsNotSuccess(t *testing.T) {
	want := errors.New("commit failed")
	m := &closeFailureFS{Memory: vfs.NewMemory(1024), err: want}
	b := newShell(t, shale.Options{Mounts: []shale.Mount{{Path: "/data", FS: m}}})
	for _, script := range []string{"printf data > /data/file", "exit 0 > /data/file", "f() { return 0; }; f > /data/file"} {
		r, err := b.Exec(t.Context(), script)
		if !errors.Is(err, want) || r.ExitCode == 0 {
			t.Fatalf("lost writeback failure: %s: %#v, %v", script, r, err)
		}
	}
	if m.closed != 3 {
		t.Fatalf("redirection closed %d times", m.closed)
	}
}

type closeFailureFS struct {
	*vfs.Memory
	err    error
	closed int
}

func (m *closeFailureFS) OpenFile(p string, flag int, perm fs.FileMode) (vfs.File, error) {
	f, err := m.Memory.OpenFile(p, flag, perm)
	if err != nil {
		return nil, err
	}
	return &closeFailureFile{File: f, owner: m}, nil
}

type closeFailureFile struct {
	vfs.File
	owner *closeFailureFS
}

func (f *closeFailureFile) Close() error { f.owner.closed++; f.File.Close(); return f.owner.err }

func TestPipelinesAndSubstitutionUnderPressure(t *testing.T) {
	b := newShell(t, shale.Options{Timeout: 2 * time.Second, MaxOutputBytes: 4096})
	for range 8 {
		for _, script := range []string{"yes | cat | head -n 1", "printf hello | false | cat", "exit 9 | cat | cat"} {
			r, err := b.Exec(t.Context(), script)
			if err != nil || r.ExitCode != 0 {
				t.Fatalf("pipeline did not drain: %q: %#v, %v", script, r, err)
			}
		}
	}
	// Both stages share command-substitution output through explicit fd
	// duplication; this used to race the expansion buffer and byte counter.
	r, err := b.Exec(t.Context(), `x=$({ seq 1 100 >&2 | seq 1 100; } 2>&1); printf '%s' "$x" | wc -l`)
	if err != nil || r.Stdout != "199\n" {
		t.Fatalf("shared substitution output: %#v, %v", r, err)
	}
	_, err = b.Exec(t.Context(), `x=$(yes); echo must-not-run`)
	if !errors.Is(err, shale.ErrExecutionLimit) {
		t.Fatalf("substitution limit swallowed by WASI: %v", err)
	}
	r, err = b.Exec(t.Context(), "echo still-usable")
	if err != nil || r.Stdout != "still-usable\n" {
		t.Fatalf("recovery: %#v, %v", r, err)
	}
}
