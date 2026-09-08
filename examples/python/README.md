# A virtual Python command

Registers `python` and `python3` using Shale's `Options.Commands` and
[`go-pyodide`](https://github.com/adrianliechti/go-pyodide). Both the shell tools
and CPython run as embedded WASM; no host Python executable is used.

This example has its own Go module so Shale users do not need the Python
dependency. It requires Go 1.27.1 and a sibling checkout at `../go-pyodide`
relative to the Shale repository. The two `replace` directives in `go.mod`
use those local sources.

```sh
cd examples/python
go run .
```

Expected output:

```text
2:pear
/work hello
```

Pass a shell script with `-c`:

```sh
go run . -c 'printf "hello\n" | python -c "import sys; print(sys.stdin.read().upper(), end=\"\")"'
go run . -c 'echo "print(6 * 7)" > answer.py; python answer.py'
go run . -c 'echo "{\"answer\":42}" | python3 -m json.tool'
go run . -c 'python -c "import sys; sys.exit(7)"; echo $?'
```

The handler forwards arguments, exported environment variables, the execution
context, and redirected stdin/stdout/stderr. A small Python launcher establishes
the shell's current working directory and supports `-c CODE`, `-m MODULE`, a
script filename, `-` (or no arguments) for stdin, and `--version`/`-V`. It is not
a full CPython command-line parser or interactive REPL. Each invocation starts
a fresh interpreter; the compiled module is reused.

Python sees a live **read-only** view of the shell filesystem, including memory
and host mounts. The interpreter's standard library, site-packages, devices,
and private writable `/tmp` keep their Pyodide mounts. Python can read files
created by earlier shell commands and import sibling modules. To persist
Python output, use shell redirection (`python ... > result.txt`). Direct Python
writes to shell files are unavailable because Pyodide's `Mount.FS` API is
read-only; its `Mount.Dir` API can separately expose a host directory for writes.

Python exit statuses become shell exit statuses, so `&&`, `||`, and `$?` work.
Go errors and cancellation remain execution errors. The example caps each
Python instance at 128 MiB; Shale's WASM memory setting applies to its own
embedded tools, while custom handlers configure their own runtime limits.

```sh
go test ./...
```
