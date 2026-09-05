package shale_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	shale "github.com/adrianliechti/shale"
)

// This oracle only executes these fixed, reviewed fixtures in a fresh temporary
// directory. It never passes fuzzed or user-provided scripts to the host shell.
// Expected results are pinned as well, so coverage remains when Bash is absent.
func TestBashDifferential(t *testing.T) {
	b := newShell(t, shale.Options{})
	bash, _ := exec.LookPath("bash")
	for _, tc := range []struct {
		name, script, want string
		code               int
	}{
		{"prefix builtin state", `mkdir prefix_cd; X=temp cd prefix_cd; printf '%s:%s\n' "${PWD##*/}" "${X-unset}"`, "prefix_cd:unset\n", 0},
		{"prefix function state", `f() { y=changed; X=inside; }; X=outer; X=temp f; printf '%s:%s\n' "$X" "$y"`, "outer:changed\n", 0},
		{"prefix expansion", `X=before; X=after printf '%s\n' "$X"; printf '%s\n' "$X"`, "before\nbefore\n", 0},
		{"nested shell export", `localvar=secret; export shared=yes; sh -c 'printf "%s:%s\n" "${localvar-unset}" "$shared"'`, "unset:yes\n", 0},
		{"nested shell args", `f() { sh -c 'printf "%s:%s\n" "$#" "${1-unset}"'; }; f secret`, "0:unset\n", 0},
		{"nested shell function", `f() { printf leaked; }; sh -c 'f 2>/dev/null'; printf '%s\n' "$?"`, "127\n", 0},
		{"positional restoration", `f() { g second; printf '%s\n' "$1"; }; g() { printf '%s\n' "$1"; }; f first`, "second\nfirst\n", 0},
		{"pipeline left exit", `exit 7 | cat; printf after`, "after", 0},
		{"pipeline right exit", `printf hi | exit 3; printf '%s\n' "$?"`, "3\n", 0},
		{"pipeline function return", `f() { return 4; }; f | cat; printf '%s\n' "$?"`, "0\n", 0},
		{"pipeline state", `x=old; { x=new; printf text; } | cat; printf '%s\n' "$x"`, "textold\n", 0},
		{"pipeline last status", `false | true; printf '%s\n' "$?"; true | false; printf '%s\n' "$?"`, "0\n1\n", 0},
		{"pipe stderr order", `f() { printf out; printf err >&2; }; f 2>pipe_err |& cat; cat pipe_err`, "outerr", 0},
		{"here string spaces", `x='a b'; cat <<< $x`, "a b\n", 0},
		{"here string glob", `touch here_match; cat <<< here_*`, "here_*\n", 0},
		{"heredoc strip tabs", "cat <<-EOF\n\thello\n\tEOF", "hello\n", 0},
		{"redirect word order", `x=before; printf '%s\n' "$x" > "${x:=file}"; cat before`, "before\n", 0},
		{"redirect expansion side effect", `unset x; printf '<%s>\n' "$x" > "${x:=redir_target}"; cat redir_target`, "<>\n", 0},
		{"redirect substitution status", `> "$(printf redir_status; exit 7)"; printf '%s\n' "$?"`, "7\n", 0},
		{"assignment substitution last", `a=$(false) b=$(true); printf '%s\n' "$?"; a=$(true) b=$(false); printf '%s\n' "$?"`, "0\n1\n", 0},
		{"empty command substitution", `$(false); printf '%s\n' "$?"`, "1\n", 0},
		{"substitution status visibility", `false; printf '%s\n' "$(printf '%s' "$?")"`, "1\n", 0},
		{"substitution strips newline", `x=$(printf 'a\n\n'); printf '<%s>\n' "$x"`, "<a>\n", 0},
		{"substitution exit", `x=$(exit 5); printf '%s\n' "$?"; printf after`, "5\nafter", 0},
		{"empty argument", `printf '<%s>\n' '' a""b "${absent-}"`, "<>\n<ab>\n<>\n", 0},
		{"parameter assignment", `unset x; printf '%s:%s\n' "${x:=default}" "$x"`, "default:default\n", 0},
		{"glob empty", `printf '<%s>\n' absolutely_missing_*`, "<absolutely_missing_*>\n", 0},
		{"glob PWD spoof", `touch glob_real; PWD=/; printf '%s\n' glob_*`, "glob_real\n", 0},
		{"PWD value survives glob setup", `PWD=/imaginary; printf '%s\n' "$PWD"`, "/imaginary\n", 0},
		{"glob PWD unset", `touch glob_unset; unset PWD; printf '%s\n' glob_un*`, "glob_unset\n", 0},
		{"loop break", `for x in a b; do printf '%s' "$x"; break; done; printf end`, "aend", 0},
		{"loop continue", `for x in a b; do printf '%s' "$x"; continue; printf no; done`, "ab", 0},
		{"arithmetic state", `x=1; ((x+=2)); printf '%s\n' "$x"`, "3\n", 0},
		{"negation", `! true; printf '%s\n' "$?"; ! false; printf '%s\n' "$?"`, "1\n0\n", 0},
		{"export after assignment", `x=one; export x; x=two; printenv x`, "two\n", 0},
		{"export empty declaration", `export empty; empty=value; printenv empty`, "value\n", 0},
		{"cd empty", `cd ''; printf '%s\n' "$?"`, "0\n", 0},
		{"fd left to right", `{ printf out; printf err >&2; } 2>&1 > fd_file; cat fd_file`, "errout", 0},
		{"fd both to file", `{ printf out; printf err >&2; } > fd_both 2>&1; cat fd_both`, "outerr", 0},
		{"quoted delimiter", "x=expanded; cat <<'EOF'\n$x\nEOF", "$x\n", 0},
		{"tabs from heredoc expansion", "x='\tkeep'; cat <<-EOF\n\t$x\n\tEOF", "\tkeep\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := "(\n" + tc.script + "\n)"
			if bash != "" && tc.name != "pipe stderr order" { // |& requires Bash 4; macOS ships Bash 3.
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-c", script)
				cmd.Dir = t.TempDir()
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/work", "LC_ALL=C", "TZ=UTC"}
				var stdout, stderr bytes.Buffer
				cmd.Stdout = &stdout
				cmd.Stderr = &stderr
				err := cmd.Run()
				code := 0
				if err != nil {
					var ee *exec.ExitError
					if !errors.As(err, &ee) {
						t.Fatal(err)
					}
					code = ee.ExitCode()
				}
				if stdout.String() != tc.want || code != tc.code {
					t.Fatalf("incorrect oracle fixture: stdout %q, stderr %q, code %d; want %q/%d", stdout.String(), stderr.String(), code, tc.want, tc.code)
				}
			}
			r, err := b.Exec(t.Context(), script)
			if err != nil || r.Stdout != tc.want || r.ExitCode != tc.code || r.Exited {
				t.Fatalf("script: %s\ngot %#v, %v; want %q/%d", tc.script, r, err, tc.want, tc.code)
			}
		})
	}
}

func TestRejectedSyntaxHasNoSideEffects(t *testing.T) {
	b := newShell(t, shale.Options{})
	for _, unsupported := range []string{`cat 0>bad`, `cat 1<input`, `cat 2<<<x`, `cat 0<&1`, `cat >ok 1<>rw`, `cat >ok {fd}>dynamic`, `x=(a b)`, `[[ true ]]`, `echo <(echo x)`, `echo hi &`, `() ((A000))`} {
		t.Run(unsupported, func(t *testing.T) {
			r, err := b.Exec(t.Context(), `printf bad > sentinel; `+unsupported)
			if err == nil || r.ExitCode != 2 {
				t.Fatalf("unsupported input not rejected at parse time: %#v, %v", r, err)
			}
			if data, err := b.ReadFile(t.Context(), "sentinel"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("validation ran after mutation: %q, %v", data, err)
			}
		})
	}
}

func TestLargeExpansionFailsWithoutHostPanic(t *testing.T) {
	b := newShell(t, shale.Options{Timeout: 2 * time.Second})
	for _, script := range []string{
		`echo {1..1000000000}`,
		`x=x; for n in {1..24}; do x=$x$x; done`,
		`f() { f; }; f`,
		`x=$(yes)`,
		strings.Repeat("( ", 200) + ":" + strings.Repeat(" )", 200),
	} {
		_, err := b.Exec(t.Context(), script)
		if !errors.Is(err, shale.ErrExecutionLimit) {
			t.Errorf("expected execution limit for %.80q: %v", script, err)
		}
	}
}
