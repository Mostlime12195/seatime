package common

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Headless detection for every hook adapter: `claude -p`, `codex exec`,
// `gemini -p`, `opencode run` and `cursor-agent -p` are not interactive
// sessions. Hooks are spawned by the harness (sometimes through a shell), so
// the harness command line is one of the first ancestors: we look at up to
// maxAncestors of them.
//
// This is a cheap client-side filter for honest setups and copy-pasted
// loops. It is not a security boundary: a patched SeaTime or a renamed
// binary passes it.

const maxAncestors = 4

// HeadlessMatcher reports whether one ancestor's argv is a non-interactive
// harness run.
type HeadlessMatcher func(args []string) bool

// IgnoreEnv reports the explicit opt-out SEATIME_IGNORE=1.
func IgnoreEnv() bool { return os.Getenv("SEATIME_IGNORE") == "1" }

// Headless reports whether this hook process runs under a non-interactive
// harness invocation that m recognizes (or SEATIME_IGNORE=1).
func Headless(m HeadlessMatcher) bool {
	if IgnoreEnv() {
		return true
	}
	for _, args := range ancestorArgs(maxAncestors) {
		if len(args) > 0 && m(args) {
			return true
		}
	}
	return false
}

// SubcommandMatcher matches argv whose program (argv[0] or, for node/bun/
// deno-launched CLIs, a script argument) has prog in its base name and that
// has any of flags among its arguments ("exec", "-p", "--print", ...; a flag
// "--x" also matches "--x=value").
func SubcommandMatcher(prog string, flags ...string) HeadlessMatcher {
	return func(args []string) bool {
		isProg := false
		for i, a := range args {
			if i > 2 { // the program is argv[0], or the script right after an interpreter (and its one flag)
				break
			}
			if strings.Contains(strings.ToLower(filepath.Base(a)), prog) {
				isProg = true
				break
			}
		}
		if !isProg {
			return false
		}
		for _, a := range args[1:] {
			if a == "--" {
				break
			}
			for _, f := range flags {
				if a == f || (strings.HasPrefix(f, "--") && strings.HasPrefix(a, f+"=")) {
					return true
				}
			}
		}
		return false
	}
}

// ancestorArgs returns the argv of up to n ancestor processes, parent first.
// Linux reads /proc; macOS and the BSDs ask ps; elsewhere (Windows) nil.
func ancestorArgs(n int) [][]string {
	var out [][]string
	pid := os.Getppid()
	for i := 0; i < n && pid > 1; i++ {
		args, ppid := procInfo(pid)
		if args == nil {
			break
		}
		out = append(out, args)
		pid = ppid
	}
	return out
}

func procInfo(pid int) ([]string, int) {
	switch runtime.GOOS {
	case "linux", "android":
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		if err != nil {
			return nil, 0
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		st, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return args, 0
		}
		// pid (comm) state ppid ...; comm may contain spaces and parentheses
		s := string(st)
		if i := strings.LastIndexByte(s, ')'); i >= 0 {
			f := strings.Fields(s[i+1:])
			if len(f) >= 2 {
				pp, _ := strconv.Atoi(f[1])
				return args, pp
			}
		}
		return args, 0
	case "darwin", "freebsd", "openbsd", "netbsd":
		raw, err := exec.Command("ps", "-o", "ppid=", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return nil, 0
		}
		f := strings.Fields(strings.TrimSpace(string(raw)))
		if len(f) < 2 {
			return nil, 0
		}
		pp, _ := strconv.Atoi(f[0])
		return f[1:], pp
	}
	return nil, 0
}
