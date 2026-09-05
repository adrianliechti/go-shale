# Shale

A Go-owned virtual shell with **uutils coreutils, grep, find, diff, and sed
running in WebAssembly**. Package: `shale`. Command: `cmd/shale`.

The shell creates its own filesystem layout. Mount ordinary Go `fs.FS` values
read-only, or use `vfs.WriteFS` for read-write access. Mounts do not need to be at
`/`; the CLI mounts the current host directory at `/workspace`.

```text
/
├── bin/          read-only uutils commands
├── usr/bin/      the same commands
├── tmp/          writable, in-memory scratch space
├── work/         default working directory for the Go API
├── dev/null
└── workspace/    CLI's host-directory mount; writes persist
```

`cat`, `/bin/cat`, and `/usr/bin/cat` invoke the embedded WASM command.
`PATH` defaults to `/usr/bin:/bin`. The command files expose the actual multicall
WASM bytes, shared in memory, with mode `0555`. These directories are reserved;
commands cannot be replaced and arbitrary mounted executables are not run.

## Try it

Requires Go 1.26 or newer. The checked-in WASM artifact means ordinary Go builds
need neither Rust nor a system-installed shell or coreutils.

```sh
go run ./cmd/shale
go run ./cmd/shale -c 'pwd; ls /bin'
go run ./cmd/shale -c 'printf "pear\napple\npear\n" | sort | uniq > result.txt; cat result.txt'
go run ./cmd/shale -root ./some-directory -readonly -c 'grep -rn TODO . | head -n 5'
go run ./cmd/shale -c 'find . -name "*.txt" | xargs sed -i "s/pear/plum/"; diff -u result.txt /dev/null'
go build ./cmd/shale
```

The third example **writes `result.txt` in your current host directory**. The
interactive prompt is intentionally basic; output is captured until each script
finishes. Piped input without `-c` is treated as a script; with `-c` it becomes
the command's stdin. Use `-timeout 30s` to change the per-script deadline.

## Go API

```go
package main

import (
    "context"
    "fmt"

    shale "github.com/adrianliechti/shale"
    "github.com/adrianliechti/shale/vfs"
)

func main() {
    ctx := context.Background()
    work := vfs.NewMemory(16 << 20)
    sh, err := shale.New(ctx, shale.Options{
        Mounts: []shale.Mount{{Path: "/workspace", FS: work}},
        Cwd: "/workspace",
    })
    if err != nil { panic(err) }
    defer sh.Close(ctx)

    result, err := sh.Exec(ctx, `echo hello > greeting.txt; cat greeting.txt`)
    if err != nil { panic(err) }
    fmt.Print(result.Stdout)
    // Check result.ExitCode for command failure (distinct from a Go error).
}
```

For write-through access to a host directory, replace `work` with
`vfs.OpenDirectory("./project")` and defer its `Close`. It uses `os.Root` to
confine filesystem operations to that directory. The caller owns mounts; close
the shell before closing its backends.

Plain `fs.FS` values, including `embed.FS`, can be mounted at any non-overlapping
absolute directory such as `/data`. They are read-only. `Mount.ReadOnly` also
turns a writable backend into a read-only mount. Root and overlapping mounts
are currently rejected.

`fs.FS` has no write operations. The extension is deliberately small:

```go
type File interface {
    fs.File
    io.Writer
}

type WriteFS interface {
    fs.FS
    OpenFile(name string, flag int, perm fs.FileMode) (File, error)
    Mkdir(name string, perm fs.FileMode) error
    Remove(name string) error
    Rename(oldName, newName string) error
}
```

Names passed to backends use `io/fs` conventions: relative paths and `.` for the
mount root. Seek, positioned I/O, truncate, timestamps, and symlink inspection
are optional capabilities. `vfs.Memory` and `vfs.Directory` implement the common
operations. There is no automatic copy-on-write overlay.

Sessions retain files, variables, functions, and cwd between calls. Calls on one
shell are serialized. `Run(ctx, Request{Script: ..., Stdin: ...})` supplies stdin;
`ReadFile` retrieves a guest file. `Result.Exited` tells interactive callers that
the script requested `exit`; the API session remains reusable.

## Compatibility

This is an initial implementation, **not full Bash or a complete POSIX/GNU
conformance claim**. Shell syntax and expansion come from `mvdan.cc/sh/v3`;
execution is our own Go implementation, not its host-executing interpreter.
The command implementations come from the uutils projects
[coreutils 0.11.0](https://github.com/uutils/coreutils/tree/0.11.0),
[grep 0.2.0](https://github.com/uutils/grep/tree/0.2.0),
[findutils 0.10.0](https://github.com/uutils/findutils/tree/0.10.0),
[diffutils v0.5.0](https://github.com/uutils/diffutils/tree/v0.5.0), and
[sed 0.2.0](https://github.com/uutils/sed/tree/0.2.0). grep and sed are early
upstream releases; see [the artifact notes](internal/wasm/README.md) for the
local patches applied to diffutils and sed.

Supported shell features include quoting, variables and exports, parameter and
arithmetic expansion, command substitution, globs, pipelines, `&&`/`||`, ordinary
redirects, heredocs, blocks, subshells, functions, `if`, `for`, and `while`/`until`.
Builtins include `cd`, `pwd`, `export`, `unset`, `exit`, `xargs`, and
single-level loop control. Nested `sh -c`/`bash -c` use this same limited language.

Not yet supported: job control/background jobs, process substitution, arrays,
`[[ ... ]]`, `case`, C-style loops, arbitrary file descriptors, `source`, `eval`,
shell options such as `set -e`/`pipefail`, external scripts, or arbitrary native
or WASM executables. Unsupported constructs return an error.

The pinned coreutils `feat_wasm` build contains 77 utilities; `coreutils --list`
prints them. The separate modules add `grep`, `find`, `diff`, `cmp`, and `sed`.
`shale.Commands()` lists every command and `shale.Versions()` the pinned
releases. Not all native coreutils are available in the WASI build (for
example, `chmod` and `stat` are absent), and `awk` is absent because the uutils
implementation has no release yet.

`xargs` is a shell builtin rather than the findutils binary, because WASI
cannot spawn processes: it supports `-0`, `-n N`, `-I R`, `-r`, `-t`, and `--`,
and runs the assembled command lines through the shell's own lookup with an
empty stdin. For the same reason `find`'s `-exec`, `-execdir`, and `-ok` print
an error for each match yet still exit 0; pipe into `xargs` instead. `grep -r`
and `sed -i` work on any writable mount.

Important filesystem/WASI limitations:

- The WASM `/usr/bin/pwd` currently fails to resolve the CLI's mounted
  host-directory cwd. Use the shell's built-in `pwd`, which works correctly.
- WASI does not expose POSIX permission bits or uid/gid. In upstream uutils,
  `test -x` always returns false; `test -w` is not a reliable mount-writability
  check. Actual writes are enforced by the Go mount layer. `ls /bin` and
  `test -f /bin/cat` work, as does `/bin/cat file`.
- Creating hard links and symlinks is not implemented in the bridge. Existing
  relative symlinks in directory mounts work within that mount; absolute or
  escaping links are rejected by `os.Root`. Guest paths are cleaned lexically,
  so `symlink/..` is not full POSIX physical-path traversal.
- Rename is limited to one mount. Cross-mount `mv` currently fails; use `cp`
  followed by `rm`. Wazero's experimental errno set cannot express `EXDEV` or
  `ENOSPC` precisely, so some diagnostics use a generic error.
- Directory mounts inherit `os.Root.Rename`'s refusal to replace an existing
  directory. Memory mounts support POSIX empty-directory replacement. File-handle
  timestamp updates use the handle's inode, not its original pathname; Unix
  directory backends currently use microsecond precision for these updates.
- Optional operations depend on the backend; command availability does not
  imply every flag works. Memory filesystem modes are metadata, not a multi-user
  permission system. No shell umask or ownership model is implemented.

## Boundaries and limits

There is no host process execution, host environment inheritance, or network
API. Only explicitly mounted backends are exposed. WASM commands get fresh
instances, a fixed environment, real time, and a cryptographic random source.

Defaults: 10 seconds per execution, 1 MiB combined stdout/stderr, 1 MiB script
and stdin limits, 10,000 shell execution steps, and 128 MiB linear memory per
WASM command. The default memory filesystem has a 64 MiB content budget;
each `vfs.NewMemory` has its own budget and a 10,000-entry cap. Limits can be
configured in `Options` where exposed. File changes are **not rolled back** on
failure, output limits, or cancellation.

This is not an audited security boundary for hostile multitenant workloads.
Per-command memory limits are not a total Go-process memory limit; pipelines
can run several instances. Backends are trusted Go code and must be safe for
concurrent use. A blocking backend operation cannot necessarily be interrupted
by a context deadline. Directory mounts have no disk quota and may expose
pre-existing hard links, devices, FIFOs, or other special files: use a dedicated
regular-file directory or memory backend for untrusted workloads, and apply
process-level resource limits as needed. Read-only directory access may still
affect host access times. Do not mount more host data than the script needs.

## Development

```sh
go test ./...
go test -race ./...
go vet -stdmethods=false ./...
```

Tests exercise actual uutils WASM, persistent writes, filesystem conformance,
command discovery, pipelines, read-only mounts, host-symlink confinement,
same-file copy protection, quotas, and cancellation. The hard-test suite includes
fixed Bash differential cases, 3,600 randomized filesystem operations (with
documented Go/POSIX differences excluded), open-inode lifetime checks, writeback
failure injection, queued deadlines, concurrent session calls, and integer-limit
boundaries. Fixed fixtures may run against an installed Bash in temporary
directories; generated scripts never run on the host. These are our regression
tests, not a run of the full upstream GNU compatibility suite.

The fuzz targets cover parsing, virtual execution, and memory-filesystem state
transitions. Crashing inputs are retained in `testdata/fuzz` and replay during
ordinary tests. For longer local runs:

```sh
go test ./internal/shell -run '^$' -fuzz '^FuzzParse$' -fuzztime=30s
go test ./internal/shell -run '^$' -fuzz '^FuzzExecution$' -fuzztime=30s
go test ./vfs -run '^$' -fuzz '^FuzzMemoryTransitions$' -fuzztime=30s
```

The `stdmethods` vet check is disabled because wazero's required
`Seek(int64, int) (int64, sys.Errno)` signature intentionally differs from
`io.Seeker`. Other vet analyzers remain enabled.

See [the artifact build notes](internal/wasm/README.md) for pinned sources,
toolchain, checksums, the working-directory constructor, and local patches.
