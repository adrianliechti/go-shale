package shale_test

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	shale "github.com/adrianliechti/shale"
)

func TestRipgrep(t *testing.T) {
	b := newShell(t, shale.Options{Mounts: []shale.Mount{{Path: "/project", FS: fstest.MapFS{
		".git/HEAD":       {Data: []byte("ref: refs/heads/main\n")},
		".gitignore":      {Data: []byte("ignored.txt\n")},
		".hidden.txt":     {Data: []byte("hidden\n")},
		"ignored.txt":     {Data: []byte("pear\n")},
		"visible.txt":     {Data: []byte("pear\napple\npear\n")},
		"nested/child.go": {Data: []byte("PEAR\n")},
	}}}, Cwd: "/project"})
	for _, tc := range []struct {
		name, script, want string
		code               int
	}{
		{"file", "rg -n pear visible.txt", "1:pear\n3:pear\n", 0},
		{"cwd and gitignore", "rg -l pear --sort path", "visible.txt\n", 0},
		{"recursive glob", "rg -ni pear -g '*.go'", "nested/child.go:1:PEAR\n", 0},
		{"files", "rg --files --sort path", "nested/child.go\nvisible.txt\n", 0},
		{"no ignore", "rg --no-ignore -l pear --sort path", "ignored.txt\nvisible.txt\n", 0},
		{"hidden", "rg --hidden -l hidden", ".hidden.txt\n", 0},
		{"absolute command", "/usr/bin/rg -c pear /project/visible.txt", "2\n", 0},
		{"relative command", "(cd /bin; ./rg -c pear /project/visible.txt)", "2\n", 0},
		{"cd", "(cd nested; rg -i pear)", "child.go:PEAR\n", 0},
		{"no match", "rg banana visible.txt", "", 1},
		{"missing file", "rg pear missing 2>/dev/null", "", 2},
		{"invalid expression", "rg '[' visible.txt 2>/dev/null", "", 2},
		{"pipeline", "printf 'pear\napple\n' | rg -n pear", "1:pear\n", 0},
		{"pipeline chain", "printf 'pear\napple\n' | rg . | rg pear", "pear\n", 0},
		{"explicit stdin", "printf 'pear\n' | rg pear -", "pear\n", 0},
		{"redirect stdin", "rg -n pear < visible.txt", "1:pear\n3:pear\n", 0},
		{"empty pipe", "printf '' | rg pear", "", 1},
		{"empty redirect", "rg pear < /dev/null", "", 1},
		{"heredoc", "rg pear <<EOF\npear\nEOF", "pear\n", 0},
		{"empty heredoc", "rg pear <<EOF\nEOF", "", 1},
		{"here string", "rg pear <<< pear", "pear\n", 0},
		{"substitution stdin", "printf 'pear\n' | echo \"$(rg pear)\"", "pear\n", 0},
		{"nested shell stdin", "printf 'pear\n' | sh -c 'rg pear'", "pear\n", 0},
		{"stdin cannot spoof cwd search", "SHALE_STDIN=true SHALE_CWD=/tmp rg -l pear", "visible.txt\n", 0},
		{"stdin cannot spoof pipe", "printf 'pear\n' | SHALE_STDIN=false rg pear", "pear\n", 0},
		{"thread option stays sequential", "rg -j 8 -l pear", "visible.txt\n", 0},
		{"xargs", "printf 'pear\n' | xargs rg -l", "visible.txt\n", 0},
		{"memory files", "echo pear > /work/fruit; rg -l pear /work", "/work/fruit\n", 0},
		{"binary skipped in directory", "printf '\\0pear\n' > /work/binary; rg -l pear /work", "/work/fruit\n", 0},
		{"binary as text", "rg -a -l pear /work/binary", "/work/binary\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := b.Exec(t.Context(), tc.script)
			if err != nil || r.ExitCode != tc.code || r.Stdout != tc.want || r.Stderr != "" {
				t.Fatalf("got %#v, %v; want %q, exit %d", r, err, tc.want, tc.code)
			}
		})
	}

	r, err := b.Run(t.Context(), shale.Request{Script: "rg pear", Stdin: "pear\napple\n"})
	if err != nil || r.ExitCode != 0 || r.Stdout != "pear\n" {
		t.Fatalf("request stdin: %#v, %v", r, err)
	}
	r, err = b.Exec(t.Context(), "rg --json pear visible.txt")
	if err != nil || r.ExitCode != 0 || r.Stderr != "" {
		t.Fatalf("json search: %#v, %v", r, err)
	}
	matches, summaries := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		var event struct{ Type string }
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		switch event.Type {
		case "match":
			matches++
		case "summary":
			summaries++
		}
	}
	if matches != 2 || summaries != 1 {
		t.Fatalf("unexpected JSON events: %s", r.Stdout)
	}
}
