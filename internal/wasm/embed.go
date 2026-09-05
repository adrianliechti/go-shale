// Package wasm contains the pinned uutils WASI executables and maps command
// names to the module that implements them.
package wasm

import (
	_ "embed"
	"sort"
	"strings"
)

//go:embed coreutils.wasm
var Coreutils []byte

//go:embed grep.wasm
var grep []byte

//go:embed find.wasm
var find []byte

//go:embed diffutils.wasm
var diffutils []byte

//go:embed sed.wasm
var sed []byte

// Version is the pinned uutils coreutils release.
const Version = "0.11.0"

// Module is one embedded WASI executable and the commands it provides.
type Module struct {
	Name     string // artifact name; <Name>.wasm and <Name>.sha256 in this package
	Project  string // upstream repository under github.com/uutils
	Version  string
	Wasm     []byte
	Commands []string
	// Multicall modules take the command name as their first argument and
	// are themselves invocable by Name. Other modules dispatch on argv[0].
	Multicall bool
}

// Argv converts a shell command line into the module's argument vector.
func (m *Module) Argv(args []string) []string {
	if m.Multicall && args[0] != m.Name {
		return append([]string{m.Name}, args...)
	}
	return args
}

// Modules lists every embedded executable. The coreutils inventory is the
// feat_wasm set of the pinned release; the others are single-purpose builds.
var Modules = []*Module{
	{Name: "coreutils", Project: "coreutils", Version: Version, Wasm: Coreutils, Commands: sorted(append([]string{"["}, strings.Fields(coreutilsCommands)...)), Multicall: true},
	{Name: "grep", Project: "grep", Version: "0.2.0", Wasm: grep, Commands: []string{"grep"}},
	{Name: "find", Project: "findutils", Version: "0.10.0", Wasm: find, Commands: []string{"find"}},
	{Name: "diffutils", Project: "diffutils", Version: "0.5.0", Wasm: diffutils, Commands: []string{"cmp", "diff"}},
	{Name: "sed", Project: "sed", Version: "0.2.0", Wasm: sed, Commands: []string{"sed"}},
}

func sorted(names []string) []string {
	sort.Strings(names)
	return names
}

const coreutilsCommands = "arch base32 base64 basename basenc cat comm cp csplit cut date dd dir dircolors dirname echo expand expr factor false fmt fold head join link ln ls mkdir mktemp mv nl nproc numfmt od paste pathchk pr printenv printf ptx pwd readlink realpath rm rmdir seq shred shuf sleep sort split sum tail tee test touch tr true truncate tsort tty uname unexpand uniq unlink vdir wc yes cksum b2sum md5sum sha1sum sha224sum sha256sum sha384sum sha512sum"

var byCommand = func() map[string]*Module {
	m := make(map[string]*Module)
	for _, mod := range Modules {
		if mod.Multicall {
			m[mod.Name] = mod
		}
		for _, c := range mod.Commands {
			m[c] = mod
		}
	}
	return m
}()

// Lookup returns the module implementing a command name, or nil.
func Lookup(command string) *Module { return byCommand[command] }

// Commands lists every dispatchable name, sorted, including multicall launchers.
func Commands() []string {
	names := make([]string, 0, len(byCommand))
	for c := range byCommand {
		names = append(names, c)
	}
	sort.Strings(names)
	return names
}
