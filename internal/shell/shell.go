// Package shell owns execution of the supported shell language. It uses mvdan's
// parser and word expansion library, never its host-executing interpreter.
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/adrianliechti/shale/internal/fsys"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

const MaxScript = 1 << 20
const maxExpansion = 1 << 20

var ErrLimit = errors.New("shell execution limit exceeded")

type IO struct {
	In       io.Reader
	Out, Err io.Writer
	// InSet distinguishes a pipe or redirect from the default empty stdin.
	InSet bool
}
type ExecFunc func(context.Context, string, []string, map[string]string, IO) (int, error)
type Shell struct {
	FS        *fsys.Namespace
	Exec      ExecFunc
	Cwd       string
	Exited    bool
	vars      variables
	funcs     map[string]*syntax.Stmt
	status    int
	subStatus int
	args      []string
	depth     int
}
type run struct {
	ctx      context.Context
	steps    atomic.Int64
	maxSteps int64
}

// IsBuiltin identifies names reserved by the shell, including the export
// declaration handled by the parser instead of invoke.
func IsBuiltin(name string) bool {
	switch name {
	case ":", "true", "false", "cd", "pwd", "export", "unset", "exit", "return", "break", "continue", "bash", "sh", "xargs":
		return true
	}
	return false
}

func New(f *fsys.Namespace, cwd string, env map[string]string, exec ExecFunc) *Shell {
	s := &Shell{FS: f, Exec: exec, Cwd: cwd, vars: make(variables), funcs: make(map[string]*syntax.Stmt)}
	for k, v := range map[string]string{"HOME": "/work", "PATH": "/usr/bin:/bin", "TMPDIR": "/tmp", "LC_ALL": "C", "TZ": "UTC"} {
		s.vars[k] = variable(v, true)
	}
	for k, v := range env {
		s.vars[k] = variable(v, true)
	}
	s.vars["PWD"] = variable(cwd, true)
	return s
}
func (s *Shell) clone() *Shell {
	c := *s
	c.vars = maps.Clone(s.vars)
	c.funcs = maps.Clone(s.funcs)
	c.args = append([]string(nil), s.args...)
	return &c
}

func Parse(src string) (*syntax.File, error) {
	if len(src) > MaxScript {
		return nil, ErrLimit
	}
	f, e := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "script")
	if e != nil {
		return nil, e
	}
	depth, count := 0, 0
	syntax.Walk(f, func(n syntax.Node) bool {
		if e != nil {
			return false
		}
		if n == nil {
			depth--
			return true
		}
		depth++
		count++
		if depth > 128 || count > 20000 {
			e = ErrLimit
			return false
		}
		switch n := n.(type) {
		case *syntax.Stmt:
			if n.Background || n.Coprocess || n.Disown {
				e = errors.New("background jobs are not supported")
			}
		case *syntax.ProcSubst:
			e = errors.New("process substitution is not supported")
		case *syntax.Assign:
			if n.Array != nil || n.Index != nil {
				e = errors.New("array assignments are not supported")
			}
		case *syntax.FuncDecl:
			// The parser can represent an anonymous () declaration. It is not
			// an executable function in this shell and has no Name to dereference.
			if n.Name == nil || !syntax.ValidName(n.Name.Value) || n.Body == nil {
				e = errors.New("invalid function declaration")
			}
		case *syntax.ForClause:
			if n.Select {
				e = errors.New("select is not supported")
			}
			if _, ok := n.Loop.(*syntax.WordIter); !ok {
				e = errors.New("C-style for loops are not supported")
			}
		case *syntax.DeclClause:
			if n.Variant.Value != "export" {
				e = fmt.Errorf("%s is not supported", n.Variant.Value)
			}
		case *syntax.TestClause, *syntax.CaseClause, *syntax.TimeClause, *syntax.CoprocClause, *syntax.LetClause:
			e = fmt.Errorf("unsupported shell construct %T", n)
		case *syntax.Redirect:
			e = validateRedirect(n)
		}
		return e == nil
	})
	return f, e
}
func (s *Shell) Run(ctx context.Context, src string, streams IO, maxSteps int64) (int, error) {
	s.Exited = false
	f, e := Parse(src)
	if e != nil {
		return 2, e
	}
	r := &run{ctx: ctx, maxSteps: maxSteps}
	code, e := s.list(r, f.Stmts, streams)
	var control flow
	if errors.As(e, &control) && control.kind == "exit" {
		s.Exited = true
		return control.code, nil
	}
	return code, e
}
func (r *run) step() error {
	if e := r.ctx.Err(); e != nil {
		return e
	}
	if r.steps.Add(1) > r.maxSteps {
		return ErrLimit
	}
	return nil
}
func (s *Shell) list(r *run, stmts []*syntax.Stmt, streams IO) (int, error) {
	code := 0
	for _, stmt := range stmts {
		var e error
		code, e = s.stmt(r, stmt, streams)
		s.status = code
		if e != nil {
			return code, e
		}
	}
	return code, nil
}
func (s *Shell) stmt(r *run, stmt *syntax.Stmt, streams IO) (int, error) {
	return s.stmtWithPipe(r, stmt, streams, false)
}
func (s *Shell) stmtWithPipe(r *run, stmt *syntax.Stmt, streams IO, pipeAll bool) (code int, err error) {
	if e := r.step(); e != nil {
		return 1, e
	}
	s.subStatus = 0
	// Simple-command words expand before redirections and prefix assignments.
	// In particular, a redirection's ${x:=...} must not affect earlier words.
	call, simple := stmt.Cmd.(*syntax.CallExpr)
	var args []string
	if simple {
		var e error
		args, e = s.fields(r, call.Args, streams)
		if e != nil {
			return 1, e
		}
	}
	io2, closers, e := s.redirect(r, stmt.Redirs, streams)
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			if closeErr := closers[i].Close(); closeErr != nil {
				var control flow
				if err == nil || errors.As(err, &control) {
					code, err = 1, fmt.Errorf("close redirection: %w", closeErr)
				}
			}
		}
	}()
	if e != nil {
		if errors.Is(e, ErrLimit) || r.ctx.Err() != nil {
			return 1, e
		}
		fmt.Fprintln(streams.Err, "shale:", e)
		return 1, nil
	}
	// Bash's |& duplicates stderr after applying the command's redirections.
	if pipeAll {
		io2.Err = io2.Out
	}
	if simple {
		code, e = s.call(r, call, args, io2)
	} else {
		code, e = s.command(r, stmt.Cmd, io2)
	}
	if stmt.Negated && e == nil {
		if code == 0 {
			code = 1
		} else {
			code = 0
		}
	}
	s.status = code
	return code, e
}
func (s *Shell) command(r *run, cmd syntax.Command, streams IO) (int, error) {
	switch c := cmd.(type) {
	case nil:
		return s.subStatus, nil
	case *syntax.CallExpr:
		return 2, errors.New("internal error: simple command bypassed expansion")
	case *syntax.Block:
		return s.list(r, c.Stmts, streams)
	case *syntax.Subshell:
		code, e := s.clone().list(r, c.Stmts, streams)
		var f flow
		if errors.As(e, &f) && f.kind == "exit" {
			e = nil
		}
		return code, e
	case *syntax.BinaryCmd:
		if c.Op == syntax.Pipe || c.Op == syntax.PipeAll {
			return s.pipeline(r, c, streams)
		}
		code, e := s.stmt(r, c.X, streams)
		if e != nil {
			return code, e
		}
		if (c.Op == syntax.AndStmt && code == 0) || (c.Op == syntax.OrStmt && code != 0) {
			return s.stmt(r, c.Y, streams)
		}
		return code, nil
	case *syntax.IfClause:
		code, e := s.list(r, c.Cond, streams)
		if e != nil {
			return code, e
		}
		if code == 0 {
			return s.list(r, c.Then, streams)
		}
		if c.Else != nil {
			return s.command(r, c.Else, streams)
		}
		return 0, nil
	case *syntax.ForClause:
		loop := c.Loop.(*syntax.WordIter)
		items := s.args
		var e error
		if loop.InPos.IsValid() {
			items, e = s.fields(r, loop.Items, streams)
			if e != nil {
				return 1, e
			}
		}
		code := 0
		for _, item := range items {
			if e = r.step(); e != nil {
				return 1, e
			}
			if e = s.vars.Set(loop.Name.Value, variable(item, false)); e != nil {
				return 1, e
			}
			code, e = s.list(r, c.Do, streams)
			var f flow
			if errors.As(e, &f) {
				if f.kind == "break" {
					return 0, nil
				}
				if f.kind == "continue" {
					continue
				}
			}
			if e != nil {
				return code, e
			}
		}
		return code, nil
	case *syntax.WhileClause:
		code := 0
		for {
			if e := r.step(); e != nil {
				return 1, e
			}
			cond, e := s.list(r, c.Cond, streams)
			if e != nil {
				return cond, e
			}
			if (cond == 0) == c.Until {
				return code, nil
			}
			code, e = s.list(r, c.Do, streams)
			var f flow
			if errors.As(e, &f) {
				if f.kind == "break" {
					return 0, nil
				}
				if f.kind == "continue" {
					continue
				}
			}
			if e != nil {
				return code, e
			}
		}
	case *syntax.FuncDecl:
		if len(s.funcs) >= 1000 {
			return 1, ErrLimit
		}
		s.funcs[c.Name.Value] = c.Body
		return 0, nil
	case *syntax.DeclClause:
		for _, a := range c.Args {
			if a.Name == nil {
				return 2, errors.New("dynamic export arguments are not supported")
			}
			if a.Naked {
				v := s.vars[a.Name.Value]
				v.Exported = true
				s.vars[a.Name.Value] = v
			} else if e := s.assign(r, a, true, streams); e != nil {
				return 1, e
			}
		}
		return 0, nil
	case *syntax.ArithmCmd:
		n, e := expand.Arithm(s.config(r, streams), c.X)
		if e != nil {
			return 1, e
		}
		if n == 0 {
			return 1, nil
		}
		return 0, nil
	default:
		return 2, fmt.Errorf("unsupported shell construct %T", cmd)
	}
}
func (s *Shell) assign(r *run, a *syntax.Assign, export bool, streams IO) error {
	v, e := expand.Literal(s.config(r, streams), a.Value)
	if e != nil {
		return e
	}
	if a.Append {
		v = s.vars.Get(a.Name.Value).String() + v
	}
	return s.vars.Set(a.Name.Value, variable(v, export))
}
func (s *Shell) call(r *run, c *syntax.CallExpr, args []string, streams IO) (int, error) {
	if len(args) == 0 {
		for _, a := range c.Assigns {
			if e := s.assign(r, a, false, streams); e != nil {
				return 1, e
			}
		}
		return s.subStatus, nil
	}
	// Prefix assignments apply to the invoked command, after its arguments
	// have been expanded in the original environment.
	// Only the assigned variables are temporary. Builtins/functions still
	// change this shell's cwd and other variables, just as an unprefixed call.
	if len(c.Assigns) > 0 {
		before := make(map[string]expand.Variable)
		defer func() {
			for k, v := range before {
				if v.IsSet() || v.Exported {
					s.vars[k] = v
				} else {
					delete(s.vars, k)
				}
			}
		}()
		for _, a := range c.Assigns {
			if _, saved := before[a.Name.Value]; !saved {
				before[a.Name.Value] = s.vars[a.Name.Value]
			}
			if e := s.assign(r, a, true, streams); e != nil {
				return 1, e
			}
		}
	}
	return s.invoke(r, args, streams)
}
func (s *Shell) invoke(r *run, args []string, streams IO) (int, error) {
	if body := s.funcs[args[0]]; body != nil {
		if s.depth >= 64 {
			return 1, ErrLimit
		}
		old := s.args
		s.args = args[1:]
		s.depth++
		defer func() { s.args = old; s.depth-- }()
		code, e := s.stmt(r, body, streams)
		var f flow
		if errors.As(e, &f) && f.kind == "return" {
			e = nil
		}
		return code, e
	}
	switch args[0] {
	case ":", "true":
		return 0, nil
	case "false":
		return 1, nil
	case "cd":
		if len(args) > 2 {
			fmt.Fprintln(streams.Err, "cd: too many arguments")
			return 1, nil
		}
		dest := s.vars.Get("HOME").String()
		if len(args) == 2 {
			dest = args[1]
		}
		if dest == "-" {
			dest = s.vars.Get("OLDPWD").String()
		}
		dest = fsys.Resolve(s.Cwd, dest)
		i, e := s.FS.Stat(dest)
		if e != nil || !i.IsDir() {
			fmt.Fprintln(streams.Err, "cd: cannot enter", dest)
			return 1, nil
		}
		s.vars["OLDPWD"] = variable(s.Cwd, true)
		s.Cwd = path.Clean(dest)
		s.vars["PWD"] = variable(s.Cwd, true)
		return 0, nil
	case "pwd":
		if len(args) > 1 && !(len(args) == 2 && (args[1] == "-L" || args[1] == "-P")) {
			return 2, nil
		}
		_, e := fmt.Fprintln(streams.Out, s.Cwd)
		return 0, e
	case "unset":
		for _, k := range args[1:] {
			delete(s.vars, k)
		}
		return 0, nil
	case "exit", "return":
		code := s.status
		if len(args) > 2 {
			return 2, errors.New("too many arguments")
		}
		if len(args) == 2 {
			n, e := strconv.Atoi(args[1])
			if e != nil {
				return 2, e
			}
			code = int(uint8(n))
		}
		return code, flow{args[0], code}
	case "break", "continue":
		if len(args) != 1 {
			return 2, errors.New("only single-level loop control is supported")
		}
		return 0, flow{args[0], 0}
	case "bash", "sh":
		if s.depth >= 64 {
			return 1, ErrLimit
		}
		if len(args) < 3 || args[1] != "-c" {
			fmt.Fprintln(streams.Err, "shale: nested shells support -c SCRIPT")
			return 2, nil
		}
		f, e := Parse(args[2])
		if e != nil {
			return 2, e
		}
		child := New(s.FS, s.Cwd, s.exported(), s.Exec)
		child.depth = s.depth + 1
		child.vars["0"] = variable(args[0], false)
		if len(args) > 3 {
			child.args = args[4:]
			child.vars["0"] = variable(args[3], false)
		}
		code, e := child.list(r, f.Stmts, streams)
		var control flow
		if errors.As(e, &control) && control.kind == "exit" {
			e = nil
		}
		return code, e
	case "xargs":
		return s.xargs(r, args[1:], streams)
	}
	return s.external(r, args, streams)
}

// external resolves a command through PATH or an explicit /bin path and runs
// it through the registered command handler.
func (s *Shell) external(r *run, args []string, streams IO) (int, error) {
	env := s.exported()
	env["PWD"] = s.Cwd
	name := args[0]
	if strings.Contains(name, "/") {
		name = fsys.Resolve(s.Cwd, name)
		if path.Dir(name) != "/bin" && path.Dir(name) != "/usr/bin" {
			fmt.Fprintln(streams.Err, name+": command not found")
			return 127, nil
		}
		name = path.Base(name)
	} else {
		found := false
		for _, dir := range strings.Split(s.vars.Get("PATH").String(), ":") {
			p := fsys.Resolve(s.Cwd, path.Join(dir, name))
			if path.Dir(p) != "/bin" && path.Dir(p) != "/usr/bin" {
				continue
			}
			if info, err := s.FS.Stat(p); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintln(streams.Err, name+": command not found")
			return 127, nil
		}
	}
	return s.Exec(r.ctx, s.Cwd, append([]string{name}, args[1:]...), env, streams)
}

// xargs is a builtin because WASI cannot spawn processes: the command lines it
// assembles run through the shell's own command lookup, so `find | xargs grep`
// works while find's -exec cannot. Supported: -0, -n N, -I R, -r, -t, and --.
// Without -0, items are separated by blanks or newlines; quotes and backslashes
// group as in GNU xargs. Each invocation reads an empty stdin.
func (s *Shell) xargs(r *run, args []string, streams IO) (int, error) {
	var nul, skipEmpty, trace bool
	per, replace := 0, ""
	fail := func(msg string) (int, error) {
		fmt.Fprintln(streams.Err, "xargs: "+msg)
		return 1, nil
	}
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-"; i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if strings.HasPrefix(a, "--") {
			switch a {
			case "--null":
				nul = true
			case "--no-run-if-empty":
				skipEmpty = true
			case "--verbose":
				trace = true
			default:
				return fail("unsupported option " + a)
			}
			continue
		}
		flag, value := a[:2], a[2:]
		switch flag {
		case "-0":
			nul = true
		case "-r":
			skipEmpty = true
		case "-t":
			trace = true
		case "-n", "-I":
			if value == "" {
				if i+1 >= len(args) {
					return fail("option requires an argument -- '" + flag[1:] + "'")
				}
				i++
				value = args[i]
			}
			if flag == "-I" {
				replace = value
				per = 1
				continue
			}
			n, e := strconv.Atoi(value)
			if e != nil || n <= 0 {
				return fail("invalid number for -n option")
			}
			per = n
			value = ""
		default:
			return fail("unsupported option " + a)
		}
		if value != "" {
			return fail("unsupported option " + a)
		}
	}
	cmd := args[i:]
	if len(cmd) == 0 {
		cmd = []string{"echo"}
	}
	input, e := io.ReadAll(io.LimitReader(streams.In, MaxScript+1))
	if e != nil {
		return 1, e
	}
	if len(input) > MaxScript {
		return 1, ErrLimit
	}
	var items []string
	switch {
	case nul:
		items = strings.Split(strings.TrimSuffix(string(input), "\x00"), "\x00")
		if len(input) == 0 {
			items = nil
		}
	case replace != "":
		for _, line := range strings.Split(strings.TrimSuffix(string(input), "\n"), "\n") {
			if line = strings.TrimLeft(line, " \t"); line != "" {
				items = append(items, line)
			}
		}
	default:
		items, e = xargsSplit(string(input))
		if e != nil {
			return fail(e.Error())
		}
	}
	if len(items) == 0 && skipEmpty {
		return 0, nil
	}
	status := 0
	child := IO{In: strings.NewReader(""), Out: streams.Out, Err: streams.Err}
	for first := true; first || len(items) > 0; first = false {
		if e := r.step(); e != nil {
			return 1, e
		}
		batch := items
		if per > 0 && per < len(items) {
			batch = items[:per]
		}
		items = items[len(batch):]
		argv := append([]string(nil), cmd...)
		if replace != "" {
			for j := 1; j < len(argv) && len(batch) == 1; j++ {
				argv[j] = strings.ReplaceAll(argv[j], replace, batch[0])
			}
		} else {
			argv = append(argv, batch...)
		}
		if trace {
			fmt.Fprintln(streams.Err, strings.Join(argv, " "))
		}
		code, e := s.external(r, argv, child)
		if e != nil {
			return code, e
		}
		switch {
		case code == 126 || code == 127:
			return code, nil
		case code == 255:
			fmt.Fprintln(streams.Err, "xargs: "+argv[0]+": exited with status 255; aborting")
			return 124, nil
		case code != 0:
			status = 123
		}
	}
	return status, nil
}

// xargsSplit tokenizes xargs input the way GNU does without -0: blanks and
// newlines separate items, single or double quotes group text, and a backslash
// escapes the next character.
func xargsSplit(input string) ([]string, error) {
	var items []string
	var cur strings.Builder
	inItem := false
	for i := 0; i < len(input); i++ {
		c := input[i]
		switch {
		case c == '\\':
			i++
			if i == len(input) {
				return nil, errors.New("backslash at end of input")
			}
			cur.WriteByte(input[i])
			inItem = true
		case c == '\'' || c == '"':
			end := strings.IndexByte(input[i+1:], c)
			if end < 0 || strings.Contains(input[i+1:i+1+end], "\n") {
				kind := "single"
				if c == '"' {
					kind = "double"
				}
				return nil, errors.New("unmatched " + kind + " quote; by default quotes are special to xargs unless you use the -0 option")
			}
			cur.WriteString(input[i+1 : i+1+end])
			inItem = true
			i += end + 1
		case c == ' ' || c == '\t' || c == '\n':
			if inItem {
				items = append(items, cur.String())
				cur.Reset()
				inItem = false
			}
		default:
			cur.WriteByte(c)
			inItem = true
		}
	}
	if inItem {
		items = append(items, cur.String())
	}
	return items, nil
}

func (s *Shell) exported() map[string]string {
	env := make(map[string]string)
	s.vars.Each(func(k string, v expand.Variable) bool {
		if v.Exported && v.IsSet() {
			env[k] = v.String()
		}
		return true
	})
	return env
}

type flow struct {
	kind string
	code int
}

func (f flow) Error() string { return f.kind }

func (s *Shell) pipeline(r *run, c *syntax.BinaryCmd, streams IO) (int, error) {
	reader, writer := io.Pipe()
	stop := context.AfterFunc(r.ctx, func() { reader.CloseWithError(r.ctx.Err()); writer.CloseWithError(r.ctx.Err()) })
	defer stop()
	leftIO := IO{In: streams.In, Out: writer, Err: streams.Err, InSet: streams.InSet}
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	left, right := s.clone(), s.clone()
	go func() {
		code, e := left.stmtWithPipe(r, c.X, leftIO, c.Op == syntax.PipeAll)
		e = pipelineFlow(e)
		_ = writer.CloseWithError(e)
		done <- result{code, e}
	}()
	code, e := right.stmt(r, c.Y, IO{In: reader, Out: streams.Out, Err: streams.Err, InSet: true})
	e = pipelineFlow(e)
	_ = reader.Close()
	l := <-done
	if r.ctx.Err() != nil {
		return 1, r.ctx.Err()
	}
	if e != nil {
		return code, e
	}
	if l.err != nil && !errors.Is(l.err, io.ErrClosedPipe) {
		return l.code, l.err
	}
	return code, nil
}

func pipelineFlow(err error) error {
	var f flow
	if errors.As(err, &f) && f.kind == "exit" {
		return nil
	}
	return err
}

func (s *Shell) config(r *run, streams IO) *expand.Config {
	env := &environment{s: s}
	return &expand.Config{Env: env, ReadDir2: func(p string) ([]fs.DirEntry, error) {
		if e := r.step(); e != nil {
			return nil, e
		}
		entries, e := s.FS.ReadDir(fsys.Resolve(s.Cwd, p))
		if len(entries) > 10000 {
			return nil, ErrLimit
		}
		return entries, e
	}, CmdSubst: func(w io.Writer, c *syntax.CmdSubst) error {
		if s.depth >= 64 {
			return ErrLimit
		}
		child := s.clone()
		child.depth++
		out := &boundedWriter{w: w, left: maxExpansion}
		code, e := child.list(r, c.Stmts, IO{In: streams.In, Out: out, Err: streams.Err, InSet: streams.InSet})
		s.subStatus = code
		if out.err != nil {
			return out.err
		}
		var control flow
		if errors.As(e, &control) && control.kind == "exit" {
			e = nil
		}
		return e
	}}
}
func (s *Shell) fields(r *run, words []*syntax.Word, streams IO) ([]string, error) {
	var fields []string
	size := 0
	cfg := s.config(r, streams)
	env := cfg.Env
	// The pinned FieldsSeq eagerly captures PWD for globbing, then lazily
	// expands words. Give that capture the real cwd while allowing "$PWD" in
	// words to retain its ordinary (possibly assigned or unset) variable value.
	cfg.Env = globEnvironment{cfg.Env.(*environment)}
	seq := expand.FieldsSeq(cfg, words...)
	cfg.Env = env
	for field, e := range seq {
		if e != nil {
			return nil, e
		}
		if e := r.ctx.Err(); e != nil {
			return nil, e
		}
		size += len(field)
		if len(fields) >= 10000 || size > maxExpansion {
			return nil, ErrLimit
		}
		fields = append(fields, field)
	}
	return fields, nil
}

type boundedWriter struct {
	mu   sync.Mutex
	w    io.Writer
	left int
	err  error
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) > w.left {
		w.err = ErrLimit
		return 0, ErrLimit
	}
	n, e := w.w.Write(p)
	w.left -= n
	return n, e
}

type variables map[string]expand.Variable

func variable(v string, exported bool) expand.Variable {
	return expand.Variable{Set: true, Kind: expand.String, Str: v, Exported: exported}
}
func (v variables) Get(k string) expand.Variable { return v[k] }
func (v variables) Each(f func(string, expand.Variable) bool) {
	for k, val := range v {
		if !f(k, val) {
			return
		}
	}
}
func (v variables) Set(k string, val expand.Variable) error {
	if len(val.Str) > maxExpansion || len(v) > 10000 {
		return ErrLimit
	}
	old := v[k]
	val.Exported = val.Exported || old.Exported
	v[k] = val
	return nil
}

type environment struct{ s *Shell }

type globEnvironment struct{ *environment }

func (e globEnvironment) Get(k string) expand.Variable {
	if k == "PWD" {
		return variable(e.s.Cwd, true)
	}
	return e.environment.Get(k)
}

func (e *environment) Get(k string) expand.Variable {
	switch k {
	case "?":
		return variable(strconv.Itoa(e.s.status), false)
	case "#":
		return variable(strconv.Itoa(len(e.s.args)), false)
	case "@", "*":
		return expand.Variable{Set: true, Kind: expand.Indexed, List: e.s.args}
	case "$", "PPID":
		return variable("1", false)
	case "0":
		if v := e.s.vars[k]; v.IsSet() {
			return v
		}
		return variable("shale", false)
	}
	// Satisfy the expansion library's home lookup hook even for unknown users,
	// preserving their literal tilde instead of falling back to os/user.Lookup.
	if strings.HasPrefix(k, "HOME ") {
		return variable("~"+strings.TrimPrefix(k, "HOME "), false)
	}
	if i, err := strconv.Atoi(k); err == nil && i > 0 && i <= len(e.s.args) {
		return variable(e.s.args[i-1], false)
	}
	return e.s.vars.Get(k)
}
func (e *environment) Each(f func(string, expand.Variable) bool) { e.s.vars.Each(f) }
func (e *environment) Set(k string, v expand.Variable) error     { return e.s.vars.Set(k, v) }

func (s *Shell) redirect(r *run, redirs []*syntax.Redirect, streams IO) (IO, []io.Closer, error) {
	var closers []io.Closer
	for _, rd := range redirs {
		fd := 1
		if rd.Op == syntax.RdrIn || rd.Op == syntax.Hdoc || rd.Op == syntax.DashHdoc || rd.Op == syntax.WordHdoc || rd.Op == syntax.DplIn {
			fd = 0
		}
		if rd.N != nil {
			fd, _ = strconv.Atoi(rd.N.Value)
		}
		if rd.Op == syntax.Hdoc || rd.Op == syntax.DashHdoc {
			word := rd.Hdoc
			if rd.Op == syntax.DashHdoc {
				word = stripHeredocTabs(word)
			}
			body, e := expand.Document(s.config(r, streams), word)
			if e != nil {
				return streams, closers, e
			}
			if len(body) > maxExpansion {
				return streams, closers, ErrLimit
			}
			streams.In = strings.NewReader(body)
			streams.InSet = true
			continue
		}
		if rd.Op == syntax.WordHdoc {
			text, e := expand.Literal(s.config(r, streams), rd.Word)
			if e != nil {
				return streams, closers, e
			}
			if len(text) >= maxExpansion {
				return streams, closers, ErrLimit
			}
			streams.In = strings.NewReader(text + "\n")
			streams.InSet = true
			continue
		}
		words, e := s.fields(r, []*syntax.Word{rd.Word}, streams)
		if e != nil {
			return streams, closers, e
		}
		if len(words) != 1 {
			return streams, closers, errors.New("ambiguous redirect")
		}
		name := words[0]
		switch rd.Op {
		case syntax.DplOut:
			var w io.Writer
			switch name {
			case "1":
				w = streams.Out
			case "2":
				w = streams.Err
			case "-":
				w = closedWriter{}
			default:
				return streams, closers, errors.New("unsupported file descriptor")
			}
			if fd == 1 {
				streams.Out = w
			} else if fd == 2 {
				streams.Err = w
			} else {
				return streams, closers, errors.New("unsupported output descriptor")
			}
			continue
		case syntax.WordHdoc:
			streams.In = strings.NewReader(name + "\n")
			streams.InSet = true
			continue
		}
		flag := os.O_RDONLY
		switch rd.Op {
		case syntax.RdrIn:
		case syntax.RdrOut, syntax.ClbOut, syntax.RdrAll:
			flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		case syntax.AppOut, syntax.AppAll:
			flag = os.O_WRONLY | os.O_CREATE | os.O_APPEND
		default:
			return streams, closers, fmt.Errorf("unsupported redirect: %s", rd.Op)
		}
		f, e := s.FS.Open(fsys.Resolve(s.Cwd, name), flag, 0666)
		if e != nil {
			return streams, closers, e
		}
		closers = append(closers, f)
		if rd.Op == syntax.RdrIn {
			if fd != 0 {
				return streams, closers, errors.New("unsupported input descriptor")
			}
			streams.In = f
			streams.InSet = true
		} else {
			w, ok := f.(io.Writer)
			if !ok {
				return streams, closers, fs.ErrPermission
			}
			if rd.Op == syntax.RdrAll || rd.Op == syntax.AppAll {
				streams.Out = w
				streams.Err = w
			} else if fd == 1 {
				streams.Out = w
			} else if fd == 2 {
				streams.Err = w
			} else {
				return streams, closers, errors.New("unsupported output descriptor")
			}
		}
	}
	return streams, closers, nil
}

type closedWriter struct{}

func (closedWriter) Write([]byte) (int, error) { return 0, fs.ErrClosed }

func validateRedirect(rd *syntax.Redirect) error {
	fd := ""
	if rd.N != nil {
		fd = rd.N.Value
	}
	switch rd.Op {
	case syntax.RdrIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
		if fd == "" || fd == "0" {
			return nil
		}
	case syntax.RdrOut, syntax.AppOut, syntax.ClbOut, syntax.DplOut:
		if fd == "" || fd == "1" || fd == "2" {
			return nil
		}
	case syntax.RdrAll, syntax.AppAll:
		if fd == "" {
			return nil
		}
	}
	return fmt.Errorf("unsupported redirection: %s%s", fd, rd.Op)
}

// Strip only literal source tabs, before expansion. Tabs supplied by a variable
// or command substitution must survive. Do not mutate a stored function's AST.
func stripHeredocTabs(word *syntax.Word) *syntax.Word {
	copyWord := *word
	copyWord.Parts = append([]syntax.WordPart(nil), word.Parts...)
	start := true
	for i, part := range copyWord.Parts {
		lit, ok := part.(*syntax.Lit)
		if !ok {
			start = false
			continue
		}
		value := *lit
		var b strings.Builder
		for _, c := range value.Value {
			if start && c == '\t' {
				continue
			}
			b.WriteRune(c)
			start = c == '\n'
		}
		value.Value = b.String()
		copyWord.Parts[i] = &value
	}
	return &copyWord
}
