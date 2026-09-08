// This example registers embedded CPython as a virtual Shale command.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	pyodide "github.com/adrianliechti/go-pyodide"
	shale "github.com/adrianliechti/shale"
)

const demo = `
printf 'pear\napple\npear\n' | python -c 'import sys; print("".join(sorted(set(sys.stdin))), end="")' > /work/fruit.txt
python -c 'from pathlib import Path; print(Path("fruit.txt").read_text(), end="")' | rg -n pear
python -c 'import os, sys; print(os.getcwd(), sys.argv[1])' hello
`

func main() {
	script := flag.String("c", demo, "shell script to execute")
	flag.Parse()
	os.Exit(run(*script))
}

func run(script string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Compile once; each command invocation gets a fresh interpreter instance.
	rt, err := pyodide.New(ctx, pyodide.WithMemoryLimit(128<<20))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer rt.Close(context.Background())
	python := pythonCommand(rt)
	sh, err := shale.New(ctx, shale.Options{
		Timeout: 30 * time.Second,
		Commands: map[string]shale.CommandFunc{
			"python": python, "python3": python,
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer sh.Close(context.Background())

	result, err := sh.Exec(ctx, script)
	fmt.Print(result.Stdout)
	fmt.Fprint(os.Stderr, result.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return result.ExitCode
}

func pythonCommand(rt *pyodide.Runtime) shale.CommandFunc {
	return func(ctx context.Context, cmd *shale.Command) (int, error) {
		err := rt.Run(ctx, bootstrap, pyodide.RunOptions{
			Args:   append([]string{cmd.Cwd}, cmd.Args[1:]...),
			Env:    cmd.Env,
			Stdin:  cmd.Stdin,
			Stdout: cmd.Stdout,
			Stderr: cmd.Stderr,
			// Pyodide's FS mounts are read-only. Shell redirection can still
			// write Python's output to any writable Shale mount.
			Mounts: []pyodide.Mount{{Path: "/", FS: cmd.FS}},
		})
		var exit *pyodide.ExitError
		if errors.As(err, &exit) {
			return exit.Code, nil
		}
		if err != nil {
			return 1, err
		}
		return 0, nil
	}
}

// go-pyodide does not expose a per-run cwd option. This small launcher changes
// to Shale's virtual cwd before running -c, -m, a script file, or stdin.
// It implements these common modes, not the full CPython option parser.
const bootstrap = `
import os
import runpy
import sys

os.chdir(sys.argv[1])
args = sys.argv[2:]
if args and args[0] in ("--version", "-V"):
    print("Python " + sys.version.split()[0])
    sys.exit(0)
if args and args[0] in ("-c", "-m") and len(args) < 2:
    print("python: " + args[0] + " requires an argument", file=sys.stderr)
    sys.exit(2)
if args and args[0] == "-c":
    sys.argv = ["-c"] + args[2:]
    exec(compile(args[1], "<string>", "exec"), {"__name__": "__main__"})
elif args and args[0] == "-m":
    sys.argv = args[1:]
    runpy.run_module(args[1], run_name="__main__", alter_sys=True)
elif not args or args[0] == "-":
    sys.argv = ["-"] + args[1:]
    exec(compile(sys.stdin.read(), "<stdin>", "exec"), {"__name__": "__main__"})
elif args[0].startswith("-"):
    print("python: supported modes are -c CODE, -m MODULE, FILE, and -", file=sys.stderr)
    sys.exit(2)
else:
    sys.argv = args
    sys.path.insert(0, os.path.dirname(os.path.abspath(args[0])))
    runpy.run_path(args[0], run_name="__main__")
`
