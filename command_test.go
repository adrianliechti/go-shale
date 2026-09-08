package shale_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	shale "github.com/adrianliechti/shale"
)

func TestCustomCommands(t *testing.T) {
	handlers := map[string]shale.CommandFunc{
		"inspect": func(ctx context.Context, cmd *shale.Command) (int, error) {
			data, err := fs.ReadFile(cmd.FS, strings.TrimPrefix(path.Join(cmd.Cwd, cmd.Args[1]), "/"))
			if err != nil {
				return 1, err
			}
			fmt.Fprintf(cmd.Stdout, "%s:%s:%s:%s", cmd.Args[0], cmd.Cwd, cmd.Env["LABEL"], data)
			cmd.Env["LABEL"] = "changed"
			return 0, nil
		},
		"relay": func(ctx context.Context, cmd *shale.Command) (int, error) {
			_, err := io.Copy(cmd.Stdout, cmd.Stdin)
			return 0, err
		},
		"save": func(ctx context.Context, cmd *shale.Command) (int, error) {
			name := strings.TrimPrefix(path.Join(cmd.Cwd, cmd.Args[1]), "/")
			file, err := cmd.FS.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
			if err != nil {
				return 1, err
			}
			_, err = io.Copy(file, cmd.Stdin)
			return 0, errors.Join(err, file.Close())
		},
		"fail": func(ctx context.Context, cmd *shale.Command) (int, error) {
			fmt.Fprintln(cmd.Stderr, "failed")
			return 7, nil
		},
		"fscheck": func(ctx context.Context, cmd *shale.Command) (int, error) {
			for _, name := range []string{"/work", "../work", "work/../work"} {
				if _, err := cmd.FS.Open(name); !errors.Is(err, fs.ErrInvalid) {
					return 1, fmt.Errorf("invalid path %q: %v", name, err)
				}
			}
			_, err := fs.ReadDir(cmd.FS, ".")
			return 0, err
		},
	}
	b := newShell(t, shale.Options{Commands: handlers, Mounts: []shale.Mount{{Path: "/data", FS: fstest.MapFS{"file": {Data: []byte("mounted\n")}}}}})
	delete(handlers, "inspect")
	for _, tc := range []struct{ script, want string }{
		{"echo hello > greeting; LABEL=one inspect greeting", "inspect:/work:one:hello\n"},
		{"export LABEL=two; /bin/inspect greeting; echo $LABEL", "inspect:/work:two:hello\ntwo\n"},
		{"(cd /data; /usr/bin/inspect file)", "inspect:/data:two:mounted\n"},
		{"printf 'hello\n' | relay | relay | tr a-z A-Z > result; cat result", "HELLO\n"},
		{"relay < result", "HELLO\n"},
		{"printf 'saved\n' | save written; cat written", "saved\n"},
		{"(cd /tmp; save written <<< tmp; cat written)", "tmp\n"},
		{"fail 2> failure; echo $?; cat failure", "7\nfailed\n"},
		{"fail 2>/dev/null && echo bad; fail 2>/dev/null || echo good", "good\n"},
		{"test -f /bin/relay && test -f /usr/bin/relay && echo exists", "exists\n"},
		{"ls /bin | rg '^inspect$'", "inspect\n"},
		{"PATH=/missing relay; echo $?", "127\n"},
		{"PATH=/missing /bin/relay <<< absolute", "absolute\n"},
		{"(cd /bin; ./relay <<< relative)", "relative\n"},
		{"echo fake > /work/relay; /work/relay; echo $?", "127\n"},
		{"echo no > /bin/relay; echo $?", "1\n"},
		{"sh -c 'relay <<< nested'", "nested\n"},
		{"fscheck", ""},
	} {
		r, err := b.Exec(t.Context(), tc.script)
		if err != nil || r.ExitCode != 0 || r.Stdout != tc.want {
			t.Errorf("%s: got %#v, %v; want %q", tc.script, r, err, tc.want)
		}
	}
}

func TestInvalidCustomCommands(t *testing.T) {
	noop := func(context.Context, *shale.Command) (int, error) { return 0, nil }
	for _, name := range []string{"", ".", "..", "a/b", "/python", "a\\b", "a\x00b", "cat", "rg", "cd", "export", "sh", "xargs"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			b, err := shale.New(t.Context(), shale.Options{Commands: map[string]shale.CommandFunc{name: noop}})
			if err == nil {
				b.Close(t.Context())
				t.Fatal("invalid or reserved name accepted")
			}
		})
	}
	if b, err := shale.New(t.Context(), shale.Options{Commands: map[string]shale.CommandFunc{"python": nil}}); err == nil {
		b.Close(t.Context())
		t.Fatal("nil handler accepted")
	}
}

func TestCustomCommandErrorsAndLimits(t *testing.T) {
	errHandler := errors.New("handler failed")
	b := newShell(t, shale.Options{Timeout: 100 * time.Millisecond, MaxOutputBytes: 64, Commands: map[string]shale.CommandFunc{
		"wait": func(ctx context.Context, cmd *shale.Command) (int, error) {
			<-ctx.Done()
			return 1, ctx.Err()
		},
		"burst": func(ctx context.Context, cmd *shale.Command) (int, error) {
			_, err := io.WriteString(cmd.Stdout, strings.Repeat("x", 128))
			return 1, err
		},
		"broken": func(context.Context, *shale.Command) (int, error) { return 1, errHandler },
	}})
	for _, tc := range []struct {
		script string
		want   error
	}{
		{"wait; echo bad", context.DeadlineExceeded},
		{"burst; echo bad", shale.ErrOutputLimit},
		{"broken; echo bad", errHandler},
	} {
		r, err := b.Exec(t.Context(), tc.script)
		if !errors.Is(err, tc.want) || r.ExitCode != 1 || strings.Contains(r.Stdout, "bad") || len(r.Stdout) > 64 {
			t.Errorf("%s: %#v, %v; want %v", tc.script, r, err, tc.want)
		}
	}
	if r, err := b.Exec(t.Context(), "echo recovered"); err != nil || r.Stdout != "recovered\n" {
		t.Fatalf("session after errors: %#v, %v", r, err)
	}
}
