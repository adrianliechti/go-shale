package vfs

import (
	"io"
	"io/fs"
	"os"
	"testing"
)

func FuzzMemoryTransitions(f *testing.F) {
	f.Add([]byte{0, 0, 1, 5, 0, 2, 4, 1, 0, 6, 0, 0})
	f.Add([]byte("stateful filesystem mutations"))
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 4096 {
			t.Skip()
		}
		m := NewMemory(256)
		names := []string{"a", "b", "dir", "dir/a", "dir/b", "../bad", "/bad", "."}
		var opened []File
		defer func() {
			for _, h := range opened {
				h.Close()
			}
		}()
		for i := 0; i+2 < len(ops); i += 3 {
			a, b := names[int(ops[i+1])%len(names)], names[int(ops[i+2])%len(names)]
			switch ops[i] % 8 {
			case 0:
				h, err := m.OpenFile(a, os.O_CREATE|os.O_RDWR, 0644)
				if err == nil {
					opened = append(opened, h)
				}
			case 1:
				_ = m.Mkdir(a, 0755)
			case 2:
				_ = m.Rename(a, b)
			case 3:
				_ = m.Remove(a)
			case 4:
				if len(opened) > 0 {
					h := opened[int(ops[i+1])%len(opened)]
					_, _ = h.(io.WriterAt).WriteAt([]byte{ops[i+2]}, int64(ops[i+2]))
				}
			case 5:
				if len(opened) > 0 {
					_ = opened[int(ops[i+1])%len(opened)].(interface{ Truncate(int64) error }).Truncate(int64(ops[i+2]))
				}
			case 6:
				if len(opened) > 0 {
					j := int(ops[i+1]) % len(opened)
					opened[j].Close()
					opened = append(opened[:j], opened[j+1:]...)
				}
			case 7:
				_, _ = fs.ReadDir(m, a)
			}
			seen := make(map[*node]bool)
			for _, n := range m.nodes {
				seen[n] = true
			}
			for _, h := range opened {
				seen[h.(*memoryFile).n] = true
			}
			var used int64
			for n := range seen {
				used += int64(len(n.data))
			}
			if used != m.used || used > m.limit || used < 0 {
				t.Fatalf("quota invariant: actual=%d counted=%d limit=%d", used, m.used, m.limit)
			}
			for p, n := range m.nodes {
				if !n.linked {
					t.Fatal("unlinked node in namespace")
				}
				if p != "." {
					if err := m.parent(p); err != nil {
						t.Fatalf("orphaned node %q: %v", p, err)
					}
				}
			}
		}
	})
}
