package shale

import (
	"context"
	"io"
	"io/fs"
	"os"

	"github.com/adrianliechti/shale/internal/fsys"
	"github.com/adrianliechti/shale/vfs"
)

// CommandFunc implements a virtual command. Return a nonzero exit code and nil
// error for an ordinary command failure; an error aborts the script. Handlers
// run as trusted Go code and must respect ctx, finish all I/O before returning,
// and support concurrent calls from pipelines. They must not call methods on
// the executing Shell, which holds its session lock for the entire script.
// The caller owns any resources captured by a handler and closes them after
// closing the shell. WASM memory limits do not apply to Go handlers.
type CommandFunc func(ctx context.Context, cmd *Command) (exitCode int, err error)

// Command describes one invocation after expansion and redirection.
// Its fields and streams are valid only for the duration of the handler call.
type Command struct {
	Args   []string          // Args[0] is the registered command name
	Cwd    string            // absolute virtual working directory
	Env    map[string]string // exported shell variables, including prefix assignments
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// FS is a live view of the shell filesystem rooted at /. Use io/fs
	// names ("work/file.txt", not "/work/file.txt"); "." denotes the root.
	// OpenFile accepts os.O_* flags for writes; Open opens files read-only.
	// Writes follow the shell's mount permissions and filesystem limits.
	// It does not acquire the shell's session lock.
	FS vfs.WriteFS
}

type commandFS struct{ ns *fsys.Namespace }

func (f commandFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return f.ns.Open("/"+name, os.O_RDONLY, 0)
}

func (f commandFS) OpenFile(name string, flag int, perm fs.FileMode) (vfs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	file, err := f.ns.Open("/"+name, flag, perm)
	if err != nil {
		return nil, err
	}
	if writable, ok := file.(vfs.File); ok {
		return writable, nil
	}
	// Plain fs.FS mounts can still be opened with O_RDONLY.
	return commandReadOnlyFile{File: file, name: name}, nil
}

func (f commandFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	return f.ns.Stat("/" + name)
}

func (f commandFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	return f.ns.ReadDir("/" + name)
}

func (f commandFS) Mkdir(name string, perm fs.FileMode) error {
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrInvalid}
	}
	return f.ns.Mkdir("/"+name, perm)
}

func (f commandFS) Remove(name string) error {
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrInvalid}
	}
	info, err := f.ns.Lstat("/" + name)
	if err != nil {
		return err
	}
	return f.ns.Remove("/"+name, info.IsDir())
}

func (f commandFS) Rename(oldName, newName string) error {
	for _, name := range []string{oldName, newName} {
		if !fs.ValidPath(name) {
			return &fs.PathError{Op: "rename", Path: name, Err: fs.ErrInvalid}
		}
	}
	return f.ns.Rename("/"+oldName, "/"+newName)
}

type commandReadOnlyFile struct {
	fs.File
	name string
}

func (f commandReadOnlyFile) Write([]byte) (int, error) {
	return 0, &fs.PathError{Op: "write", Path: f.name, Err: fs.ErrPermission}
}

var _ vfs.WriteFS = commandFS{}
