package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pyodide "github.com/adrianliechti/go-pyodide"
	shale "github.com/adrianliechti/shale"
)

func TestPythonCommand(t *testing.T) {
	rt, err := pyodide.New(t.Context(), pyodide.WithMemoryLimit(128<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(context.Background())
	python := pythonCommand(rt)
	sh, err := shale.New(t.Context(), shale.Options{
		Timeout:  30 * time.Second,
		Commands: map[string]shale.CommandFunc{"python": python, "python3": python},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close(context.Background())
	for _, tc := range []struct {
		name, script, want string
		code               int
	}{
		{"demo", demo, "2:pear\n/work hello\n", 0},
		{"stdin script", "printf 'print(6 * 7)\n' | python -", "42\n", 0},
		{"implicit stdin", "printf 'print(42)\n' | python", "42\n", 0},
		{"file and sibling imports", "mkdir scripts; echo 'answer = 42' > scripts/helper.py; printf 'import helper, sys\nprint(helper.answer, sys.argv[1])\n' > scripts/main.py; python scripts/main.py hello", "42 hello\n", 0},
		{"module", "printf '{\"answer\":42}\n' | python3 -m json.tool", "{\n    \"answer\": 42\n}\n", 0},
		{"cwd and environment", "(cd scripts; LABEL=hello python -c 'import os; print(os.getcwd(), os.environ[\"LABEL\"])')", "/work/scripts hello\n", 0},
		{"exit code", "python -c 'import sys; sys.exit(7)'", "", 7},
		{"conditional", "python -c 'import sys; sys.exit(7)' || echo recovered", "recovered\n", 0},
		{"stderr", "python -c 'import sys; print(\"diagnostic\", file=sys.stderr)' 2>&1", "diagnostic\n", 0},
		{"missing code", "python -c 2>/dev/null", "", 2},
		{"unsupported flag", "python -Z 2>/dev/null", "", 2},
		{"readonly shell files", "python -c 'open(\"fruit.txt\", \"w\").write(\"bad\")' 2>/dev/null; cat fruit.txt", "apple\npear\n", 0},
		{"fresh instances", "python -c 'x = 42'; python -c 'print(\"x\" in globals())'", "False\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := sh.Exec(t.Context(), tc.script)
			if err != nil || r.ExitCode != tc.code || r.Stdout != tc.want || r.Stderr != "" {
				t.Fatalf("got %#v, %v; want %q, exit %d", r, err, tc.want, tc.code)
			}
		})
	}
	r, err := sh.Exec(t.Context(), "python -c 'raise ValueError(\"example error\")'")
	if err != nil || r.ExitCode != 1 || !strings.Contains(r.Stderr, "ValueError: example error") {
		t.Fatalf("python exception: %#v, %v", r, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := sh.Exec(ctx, "python -c 'while True: pass'"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("python cancellation: %v", err)
	}
	if r, err := sh.Exec(t.Context(), "python -c 'print(42)'"); err != nil || r.Stdout != "42\n" {
		t.Fatalf("after cancellation: %#v, %v", r, err)
	}
}
