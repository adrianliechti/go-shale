package wasm

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestCommandFS(t *testing.T) {
	f := FS()
	if err := fstest.TestFS(f, "cat", "ls", "coreutils", "grep", "find", "diff", "cmp", "sed", "rg"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]*Module{"cat": Lookup("coreutils"), "[": Lookup("coreutils"), "diff": Lookup("cmp"), "grep": Lookup("grep"), "rg": Lookup("rg")} {
		i, err := fs.Stat(f, name)
		if err != nil || i.Mode().Perm() != 0555 || i.Size() != int64(len(want.Wasm)) {
			t.Fatalf("%s metadata: %v, %v", name, i, err)
		}
	}
	if _, err := fs.Stat(f, "diffutils"); err == nil {
		t.Fatal("non-multicall module name must not be a command")
	}
	if !slices.IsSorted(Commands()) {
		t.Fatal("commands must be sorted")
	}
}

func TestCustomCommandFS(t *testing.T) {
	f := FS("python", "python3")
	i, err := fs.Stat(f, "python")
	if err != nil || i.Mode().Perm() != 0555 || i.Size() != 0 {
		t.Fatalf("custom command metadata: %v, %v", i, err)
	}
	b, err := fs.ReadFile(f, "python")
	if err != nil || len(b) != 0 {
		t.Fatalf("custom command contents: %q, %v", b, err)
	}
	if _, err := fs.Stat(FS(), "python"); !os.IsNotExist(err) {
		t.Fatalf("custom command leaked to other sessions: %v", err)
	}
}

func TestModules(t *testing.T) {
	seen := make(map[string]string)
	for _, m := range Modules {
		if m.Name == "" || m.Project == "" || m.Version == "" || len(m.Wasm) == 0 || len(m.Commands) == 0 {
			t.Fatalf("incomplete module %+v", m.Name)
		}
		if !slices.IsSorted(m.Commands) {
			t.Fatalf("%s: commands must be sorted", m.Name)
		}
		for _, c := range m.Commands {
			if prev, dup := seen[c]; dup {
				t.Fatalf("command %s provided by both %s and %s", c, prev, m.Name)
			}
			seen[c] = m.Name
		}
	}
	if got := Lookup("coreutils").Argv([]string{"cat", "f"}); !slices.Equal(got, []string{"coreutils", "cat", "f"}) {
		t.Fatalf("multicall argv: %q", got)
	}
	if got := Lookup("coreutils").Argv([]string{"coreutils", "--list"}); !slices.Equal(got, []string{"coreutils", "--list"}) {
		t.Fatalf("launcher argv: %q", got)
	}
	if got := Lookup("diff").Argv([]string{"diff", "a", "b"}); !slices.Equal(got, []string{"diff", "a", "b"}) {
		t.Fatalf("single argv: %q", got)
	}
}

func TestArtifactChecksums(t *testing.T) {
	for _, m := range Modules {
		b, err := os.ReadFile(m.Name + ".sha256")
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(m.Wasm)
		if strings.Fields(string(b))[0] != hex.EncodeToString(sum[:]) {
			t.Fatalf("%s: embedded WASM checksum mismatch; rebuild and update provenance", m.Name)
		}
	}
}
