package vfs

import (
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"slices"
	"testing"
)

func TestOpenDirectorySurvivesRenameAndPathReuse(t *testing.T) {
	m := NewMemory(1024)
	if err := m.Mkdir("old", 0755); err != nil {
		t.Fatal(err)
	}
	put(t, m, "old/original", "contents")
	f, err := m.Open("old")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := m.Rename("old", "new"); err != nil {
		t.Fatal(err)
	}
	if err := m.Mkdir("old", 0755); err != nil {
		t.Fatal(err)
	}
	put(t, m, "old/imposter", "wrong")
	es, err := f.(fs.ReadDirFile).ReadDir(-1)
	if err != nil || len(es) != 1 || es[0].Name() != "original" {
		t.Fatalf("open handle followed reused pathname: %v, %v", es, err)
	}
}

func TestOverwriteRenameKeepsOpenDestination(t *testing.T) {
	m := NewMemory(6)
	put(t, m, "src", "abc")
	put(t, m, "dst", "XYZ")
	f, err := m.Open("dst")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := m.Rename("src", "dst"); err != nil {
		t.Fatal(err)
	}
	old, err := io.ReadAll(f)
	if err != nil || string(old) != "XYZ" {
		t.Fatalf("overwritten open inode: %q, %v", old, err)
	}
	current, err := fs.ReadFile(m, "dst")
	if err != nil || string(current) != "abc" {
		t.Fatalf("new inode: %q, %v", current, err)
	}
	if m.used != 6 {
		t.Fatalf("lost open inode quota: %d", m.used)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if m.used != 3 {
		t.Fatalf("quota after close: %d", m.used)
	}
}

func TestMemoryFileErrorContracts(t *testing.T) {
	m := NewMemory(32)
	put(t, m, "a", "original")
	f, err := m.OpenFile("a", os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.(io.WriterAt).WriteAt([]byte("bad"), 0); err == nil {
		t.Error("WriteAt on append file must fail like os.File")
	}
	if err := f.(interface{ Truncate(int64) error }).Truncate(-1); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("negative truncate: %v", err)
	}
	b, _ := fs.ReadFile(m, "a")
	if string(b) != "original" {
		t.Fatalf("failed operation changed data: %q", b)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(nil); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("closed read: %v", err)
	}
	if _, err := f.Write(nil); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("closed write: %v", err)
	}
	if _, err := f.Stat(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("closed stat: %v", err)
	}
}

// Compare thousands of state transitions with a real os.Root. No user or fuzz
// input is interpreted as a host path: operations use this fixed name set only.
func TestMemoryStateMachineAgainstDirectory(t *testing.T) {
	for _, seed := range []uint64{1, 17, 92131} {
		t.Run(string(rune('a'+seed%26)), func(t *testing.T) {
			m := NewMemory(1 << 20)
			d, err := OpenDirectory(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			rng := rand.New(rand.NewPCG(seed, seed+1))
			names := []string{"a", "b", "c", "d", "d/x", "d/y", "e", "e/x"}
			for step := range 1200 {
				a, z := names[rng.IntN(len(names))], names[rng.IntN(len(names))]
				op, size := rng.IntN(7), rng.IntN(40)
				// os.Root.Rename deliberately rejects existing directory targets,
				// including self-renames. Memory uses POSIX semantics for these;
				// verify them separately instead of using Go as the oracle.
				if op == 2 {
					if i, err := m.Stat(z); err == nil && i.IsDir() {
						continue
					}
				}
				value := []byte{byte(step), byte(step >> 8), byte(size)}
				apply := func(f WriteFS) error {
					switch op {
					case 0:
						return f.Mkdir(a, 0755)
					case 1:
						return f.Remove(a)
					case 2:
						return f.Rename(a, z)
					default:
						flags := os.O_CREATE | os.O_RDWR
						if op == 3 {
							flags |= os.O_TRUNC
						}
						if op == 4 {
							flags |= os.O_APPEND
						}
						h, err := f.OpenFile(a, flags, 0644)
						if err != nil {
							return err
						}
						defer h.Close()
						if op == 5 {
							return h.(interface{ Truncate(int64) error }).Truncate(int64(size))
						}
						if op == 6 {
							_, err = h.(io.WriterAt).WriteAt(value, int64(size))
							return err
						}
						_, err = h.Write(value)
						return err
					}
				}
				em, ed := apply(m), apply(d)
				if (em == nil) != (ed == nil) {
					t.Fatalf("seed %d step %d op %d %s -> %s: memory %v, directory %v", seed, step, op, a, z, em, ed)
				}
				ms, ds := snapshot(t, m), snapshot(t, d)
				if !slices.Equal(ms, ds) {
					t.Fatalf("seed %d step %d op %d %s -> %s:\nmemory %q\nhost %q", seed, step, op, a, z, ms, ds)
				}
			}
		})
	}
}

func snapshot(t *testing.T, f fs.FS) []string {
	t.Helper()
	var result []string
	err := fs.WalkDir(f, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			result = append(result, p+"/")
			return nil
		}
		b, err := fs.ReadFile(f, p)
		if err != nil {
			return err
		}
		result = append(result, p+":"+string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
