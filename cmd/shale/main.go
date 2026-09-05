package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	shale "github.com/adrianliechti/shale"
	"github.com/adrianliechti/shale/internal/shell"
	"github.com/adrianliechti/shale/vfs"
	"mvdan.cc/sh/v3/syntax"
)

func main() { os.Exit(run()) }
func run() int {
	var command string
	flag.StringVar(&command, "c", "", "execute a shell script")
	root := flag.String("root", ".", "host directory mounted at /workspace (read-write by default)")
	readOnly := flag.Bool("readonly", false, "make /workspace read-only")
	timeout := flag.Duration("timeout", 10*time.Second, "maximum duration per execution")
	version := flag.Bool("version", false, "print the embedded uutils versions")
	flag.Parse()
	if *version {
		versions := shale.Versions()
		parts := make([]string, 0, len(versions))
		for _, name := range slices.Sorted(maps.Keys(versions)) {
			parts = append(parts, name+" "+versions[name])
		}
		fmt.Println("shale (uutils " + strings.Join(parts, ", ") + ")")
		return 0
	}
	if flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: shale [-root DIR] [-readonly] [-c SCRIPT]")
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	dir, err := vfs.OpenDirectory(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer dir.Close()
	b, err := shale.New(ctx, shale.Options{Mounts: []shale.Mount{{Path: "/workspace", FS: dir, ReadOnly: *readOnly}}, Cwd: "/workspace", Timeout: *timeout})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer b.Close(context.Background())
	exited := false
	exec := func(src, stdin string) int {
		res, err := b.Run(ctx, shale.Request{Script: src, Stdin: stdin})
		exited = res.Exited
		fmt.Fprint(os.Stdout, res.Stdout)
		fmt.Fprint(os.Stderr, res.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "shale:", err)
			if res.ExitCode == 0 {
				return 1
			}
		}
		return res.ExitCode
	}
	hasCommand := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "c" {
			hasCommand = true
		}
	})
	info, err := os.Stdin.Stat()
	interactive := err == nil && info.Mode()&os.ModeCharDevice != 0
	if hasCommand {
		stdin := ""
		if !interactive {
			data, e := io.ReadAll(io.LimitReader(os.Stdin, shell.MaxScript+1))
			if e != nil || len(data) > shell.MaxScript {
				fmt.Fprintln(os.Stderr, "shale: input too large or unreadable")
				return 1
			}
			stdin = string(data)
		}
		return exec(command, stdin)
	}
	if !interactive {
		data, e := io.ReadAll(io.LimitReader(os.Stdin, shell.MaxScript+1))
		if e != nil || len(data) > shell.MaxScript {
			fmt.Fprintln(os.Stderr, "shale: script too large or unreadable")
			return 1
		}
		return exec(string(data), "")
	}
	fmt.Fprintln(os.Stderr, "shale — /workspace is", *root, "(writes persist unless -readonly)")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), shell.MaxScript)
	var src strings.Builder
	code := 0
	for ctx.Err() == nil {
		prompt := "shale$ "
		if src.Len() > 0 {
			prompt = "> "
		}
		fmt.Fprint(os.Stderr, prompt)
		if !scanner.Scan() {
			break
		}
		src.WriteString(scanner.Text())
		src.WriteByte('\n')
		_, parseErr := shell.Parse(src.String())
		if syntax.IsIncomplete(parseErr) {
			continue
		}
		script := src.String()
		src.Reset()
		code = exec(script, "")
		if exited {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if src.Len() > 0 {
		code = exec(src.String(), "")
	}
	return code
}
