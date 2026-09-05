package shale_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	shale "github.com/adrianliechti/shale"
	"github.com/adrianliechti/shale/vfs"
)

func TestShellIntegration(t *testing.T) {
	ctx := context.Background()
	source := fstest.MapFS{"names.txt": {Data: []byte("pear\napple\npear\n")}}
	work := vfs.NewMemory(1 << 20)
	b, err := shale.New(ctx, shale.Options{Mounts: []shale.Mount{{Path: "/data", FS: source}, {Path: "/workspace", FS: work}}, Cwd: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	for _, tc := range []struct {
		name, script, want string
		code               int
	}{
		{"version", "coreutils --version", "", 0},
		{"shell name", "printf '%s\\n' \"$0\"", "shale\n", 0},
		{"read mount", "cat /data/names.txt", "pear\napple\npear\n", 0},
		{"pipeline redirect", "cat /data/names.txt | sort | uniq > sorted.txt; cat sorted.txt", "apple\npear\n", 0},
		{"append", "echo plum >> sorted.txt; cat sorted.txt", "apple\npear\nplum\n", 0},
		{"cwd", "mkdir sub; cd sub; echo yes > f; cat f; cd ..; cat sub/f", "yes\nyes\n", 0},
		{"mutation", "cp sorted.txt copy.txt; mv copy.txt moved.txt; cat moved.txt; rm moved.txt", "apple\npear\nplum\n", 0},
		{"quoting loop", "for x in 'a b' c; do printf '<%s>\n' \"$x\"; done", "<a b>\n<c>\n", 0},
		{"substitution", "x=$(printf hello); echo \"$x\"", "hello\n", 0},
		{"substitution status", "x=$(false)", "", 1},
		{"conditional", "if false; then echo no; else echo yes; fi", "yes\n", 0},
		{"unknown", "not_a_command", "", 127},
		{"absolute commands", "/bin/cat sorted.txt | /usr/bin/head -n 1", "apple\n", 0},
		{"binary exists", "test -f /bin/cat && test -f /usr/bin/cat", "", 0},
		{"WASI execute permission unsupported", "test -x /bin/cat", "", 1},
		{"path lookup", "PATH=/missing cat sorted.txt", "", 127},
		{"absolute ignores path", "PATH=/missing /bin/echo yes", "yes\n", 0},
		{"relative executable", "cd /bin; ./echo yes; cd /workspace", "yes\n", 0},
		{"functions", "greet() { echo \"hello $1\"; }; greet world", "hello world\n", 0},
		{"subshell state", "x=outer; (x=inner; cd /tmp); echo \"$x:$PWD\"", "outer:/workspace\n", 0},
		{"arithmetic", "echo $((2 + 3 * 4)); expr 2 + 3", "14\n5\n", 0},
		{"glob", "printf '%s\n' sub/*", "sub/f\n", 0},
		{"test brackets", "[ -d sub ] && echo yes", "yes\n", 0},
		{"touch truncate", "touch fresh; touch fresh; truncate -s 3 fresh; wc -c < fresh", "3\n", 0},
		{"stdin redirect", "cat <<EOF\nhello\nEOF", "hello\n", 0},
		{"quoted heredoc", "cat <<'EOF'\n$x\nEOF", "$x\n", 0},
		{"pipe early close", "yes | head -n 1", "y\n", 0},
		{"fd redirect", "cat missing 2>/dev/null || echo failed", "failed\n", 0},
		{"virtual cwd cannot spoof", "SHALE_CWD=/data /bin/cat sub/f", "yes\n", 0},
		{"grep", "grep -n pear /data/names.txt", "1:pear\n3:pear\n", 0},
		{"grep no match", "grep zzz sorted.txt", "", 1},
		{"grep recursive relative cwd", "grep -rl yes sub", "sub/f\n", 0},
		{"grep absolute", "/usr/bin/grep -c . /data/names.txt", "3\n", 0},
		{"grep virtual cwd cannot spoof", "SHALE_CWD=/data grep -c yes sub/f", "1\n", 0},
		{"sed first use in pipeline", "printf 'a\nb\n' | sed s/a/A/ | sed s/b/B/", "A\nB\n", 0},
		{"sed print", "sed -n 's/pear/PEAR/p' sorted.txt", "PEAR\n", 0},
		{"sed in place", "cp sorted.txt edit.txt; sed -i 's/plum/prune/' edit.txt; tail -n 1 edit.txt; rm edit.txt", "prune\n", 0},
		{"sed in place backup", "cp sorted.txt edit.txt; sed -i.bak 1d edit.txt; cat edit.txt edit.txt.bak | wc -l; rm edit.txt edit.txt.bak", "5\n", 0},
		{"sed expression flag", "sed -i -e 's/x/y/' -e 's/a/b/' /dev/null; echo ok", "ok\n", 0},
		{"find", "find . -name f -type f", "./sub/f\n", 0},
		{"find maxdepth", "find . -maxdepth 1 -type d | sort", ".\n./sub\n", 0},
		{"diff", "printf 'a\nb\n' > d1; printf 'a\nc\n' > d2; diff d1 d2", "2c2\n< b\n---\n> c\n", 1},
		{"diff unified", "diff -u d1 d2 | tail -n 4", "@@ -1,2 +1,2 @@\n a\n-b\n+c\n", 0},
		{"diff header timestamps", "diff -u d1 d2 | head -n 2 | grep -c '^[-+][-+][-+] d[12]\t[0-9][0-9-]* [0-9][0-9:.]* +0000$'", "2\n", 0},
		{"diff identical", "diff d1 d1 && echo same", "same\n", 0},
		{"cmp", "cmp d1 d2; cmp -s d1 d1 && echo same", "d1 d2 differ: char 3, line 2\nsame\n", 0},
		{"xargs batches", "printf 'one two\nthree\n' | xargs -n 2 echo", "one two\nthree\n", 0},
		{"xargs replace", "printf 'a\nb\n' | xargs -I{} echo '[{}]'", "[a]\n[b]\n", 0},
		{"xargs null", "printf 'x y\\0z\\0' | xargs -0 printf '<%s>'", "<x y><z>", 0},
		{"xargs quotes", "printf '%s\n' \"'a b' c\\\\ d\" | xargs -n 1 echo", "a b\nc d\n", 0},
		{"xargs unmatched quote", "echo \"it's\" | xargs echo 2>/dev/null; echo $?", "1\n", 0},
		{"xargs empty input", "printf '' | xargs -r echo yes; printf '' | xargs echo yes", "yes\n", 0},
		{"xargs default command", "echo a b | xargs", "a b\n", 0},
		{"xargs trace", "echo a | xargs -t echo 2>&1", "echo a\na\n", 0},
		{"xargs status", "echo missing | xargs cat 2>/dev/null; echo $?; echo x | xargs nope 2>/dev/null; echo $?", "123\n127\n", 0},
		{"xargs bad option", "echo x | xargs -P 2 echo 2>/dev/null; echo $?", "1\n", 0},
		{"find piped to xargs", "find . -name f | xargs cat", "yes\n", 0},
		{"tools in bin", "test -f /bin/grep && test -f /usr/bin/sed && test -f /bin/find && test -f /bin/diff && test -f /bin/cmp && ! test -f /bin/diffutils && ! test -f /bin/xargs", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, e := b.Exec(ctx, tc.script)
			if e != nil {
				t.Fatal(e)
			}
			if r.ExitCode != tc.code || (tc.name != "version" && r.Stdout != tc.want) {
				t.Fatalf("got %#v; want output %q, exit %d", r, tc.want, tc.code)
			}
			t.Logf("%s", r.Stdout)
		})
	}
	data, err := fs.ReadFile(work, "sorted.txt")
	if err != nil || string(data) != "apple\npear\nplum\n" {
		t.Fatalf("write-through: %q, %v", data, err)
	}
	r, err := b.Exec(ctx, "coreutils --list")
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("coreutils --list: %#v, %v", r, err)
	}
	all := shale.Commands()
	for _, c := range append(strings.Fields(r.Stdout), "coreutils", "grep", "find", "diff", "cmp", "sed") {
		if !slices.Contains(all, c) {
			t.Fatalf("command inventory lacks %s: %q", c, all)
		}
	}
	if v := shale.Versions(); v["coreutils"] != shale.CoreutilsVersion || v["grep"] == "" || v["findutils"] == "" || v["diffutils"] == "" || v["sed"] == "" {
		t.Fatalf("versions: %v", v)
	}
	r, err = b.Run(ctx, shale.Request{Script: "cat | tr a-z A-Z", Stdin: "hello\n"})
	if err != nil || r.ExitCode != 0 || r.Stdout != "HELLO\n" {
		t.Fatalf("stdin: %#v, %v", r, err)
	}
}

func newShell(t *testing.T, opts shale.Options) *shale.Shell {
	t.Helper()
	b, err := shale.New(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return b
}

func TestDirectoryAndReadOnlyMounts(t *testing.T) {
	host := t.TempDir()
	dir, err := vfs.OpenDirectory(host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dir.Close() })
	ro := fstest.MapFS{"secret": {Data: []byte("keep")}}
	b := newShell(t, shale.Options{Mounts: []shale.Mount{{Path: "/workspace", FS: dir}, {Path: "/ro", FS: ro}}, Cwd: "/workspace"})
	r, err := b.Exec(t.Context(), "mkdir d && echo original > d/a && cp d/a d/b && mv d/b d/c && rm d/a && touch d/c && cat d/c")
	if err != nil || r.ExitCode != 0 || r.Stdout != "original\n" {
		t.Fatalf("host mutations: %#v, %v", r, err)
	}
	data, err := os.ReadFile(filepath.Join(host, "d", "c"))
	if err != nil || string(data) != "original\n" {
		t.Fatalf("write-through: %q, %v", data, err)
	}
	for _, script := range []string{"echo bad > /ro/secret", "cp d/c /ro/new", "rm /ro/secret", "mkdir /ro/sub", "touch /ro/secret", "echo bad > /bin/cat", "rm /usr/bin/cat", "mv /bin /work/tools", "cat /etc/passwd", "cat /../../etc/passwd"} {
		r, err := b.Exec(t.Context(), script)
		if err != nil || r.ExitCode == 0 {
			t.Errorf("expected command failure %q: %#v, %v", script, r, err)
		}
	}
	if string(ro["secret"].Data) != "keep" {
		t.Fatal("read-only data changed")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(host, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{"cat escape", "echo bad > escape"} {
		r, err := b.Exec(t.Context(), script)
		if err != nil || r.ExitCode == 0 {
			t.Errorf("symlink escape %q: %#v, %v", script, r, err)
		}
	}
	data, err = os.ReadFile(outside)
	if err != nil || string(data) != "outside" {
		t.Fatalf("outside file changed: %q, %v", data, err)
	}
	if err := os.Symlink("d", filepath.Join(host, "alias")); err != nil {
		t.Fatal(err)
	}
	r, err = b.Exec(t.Context(), "readlink alias; cat alias/c; rm alias; cat d/c")
	if err != nil || r.ExitCode != 0 || r.Stdout != "d\noriginal\noriginal\n" {
		t.Fatalf("safe symlink: %#v, %v", r, err)
	}
	if err := os.Link(filepath.Join(host, "d", "c"), filepath.Join(host, "same")); err != nil {
		t.Fatal(err)
	}
	r, err = b.Exec(t.Context(), "cp same d/c")
	if err != nil || r.ExitCode == 0 {
		t.Fatalf("cp must reject same file: %#v, %v", r, err)
	}
	data, _ = os.ReadFile(filepath.Join(host, "same"))
	if string(data) != "original\n" {
		t.Fatal("cp destroyed hard-linked source")
	}
}

func TestLimitsAndIsolation(t *testing.T) {
	t.Setenv("SHALE_HOST_SECRET", "must-not-leak")
	b := newShell(t, shale.Options{Timeout: 300 * time.Millisecond, MaxOutputBytes: 4096, MaxSteps: 100})
	for _, tc := range []struct {
		script string
		want   error
	}{
		{"yes", shale.ErrOutputLimit},
		{"sleep 3600", context.DeadlineExceeded},
		{"sleep 3600 | cat", context.DeadlineExceeded},
	} {
		start := time.Now()
		r, err := b.Exec(t.Context(), tc.script)
		if !errors.Is(err, tc.want) {
			t.Errorf("%q: %#v, %v; want %v", tc.script, r, err, tc.want)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("%q did not stop promptly", tc.script)
		}
		if len(r.Stdout)+len(r.Stderr) > 4096 {
			t.Error("output budget exceeded")
		}
	}
	if _, err := b.Exec(t.Context(), "while true; do :; done"); err == nil {
		t.Fatal("step budget not enforced")
	}
	for _, script := range []string{"echo hi &", "cat <(echo hi)", "[[ -f /bin/cat ]]"} {
		if _, err := b.Exec(t.Context(), script); err == nil {
			t.Errorf("unsupported syntax accepted: %s", script)
		}
	}
	r, err := b.Exec(t.Context(), "printf '%s|%s|%s\n' \"$SHALE_HOST_SECRET\" ~ ~root")
	if err != nil || r.Stdout != "|/work|~root\n" {
		t.Fatalf("host environment leak: %#v, %v", r, err)
	}
	r, err = b.Exec(t.Context(), "exit 7; echo no")
	if err != nil || r.ExitCode != 7 || !r.Exited || r.Stdout != "" {
		t.Fatalf("exit: %#v, %v", r, err)
	}
	r, err = b.Exec(t.Context(), "echo recovered")
	if err != nil || r.Exited || r.Stdout != "recovered\n" {
		t.Fatalf("reuse after cancellation/exit: %#v, %v", r, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := b.Exec(ctx, "echo no"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: %v", err)
	}
}

func TestInvalidMounts(t *testing.T) {
	for _, mounts := range [][]shale.Mount{
		{{Path: "/", FS: vfs.NewMemory(1024)}},
		{{Path: "/bin", FS: vfs.NewMemory(1024)}},
		{{Path: "/usr", FS: vfs.NewMemory(1024)}},
		{{Path: "relative", FS: vfs.NewMemory(1024)}},
		{{Path: "/data"}},
	} {
		if b, err := shale.New(t.Context(), shale.Options{Mounts: mounts}); err == nil {
			b.Close(t.Context())
			t.Fatalf("invalid mounts accepted: %#v", mounts)
		}
	}
}
