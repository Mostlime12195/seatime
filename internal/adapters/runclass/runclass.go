// Package runclass classifies shell commands as a run of the user's project:
// test, build, dev (long-running dev server/watch) or exec (run a program).
// Commands that are not project runs (git, ls, cat, grep, installs...) are not
// classified. Pure, fast, no I/O.
package runclass

import (
	"path"
	"strings"

	"seatime/pkg/event"
)

// Classify returns the RunKind for a (possibly compound) shell command. When
// several segments match, test beats build beats dev beats exec.
func Classify(command string) (kind string, ok bool) {
	best := 0
	for _, seg := range segments(command, 0) {
		if k := classifySegment(seg); k != "" {
			if r := rank(k); r > best {
				best, kind = r, k
			}
		}
	}
	return kind, best > 0
}

func rank(k string) int {
	switch k {
	case event.RunTest:
		return 4
	case event.RunBuild:
		return 3
	case event.RunDev:
		return 2
	case event.RunExec:
		return 1
	}
	return 0
}

// segments splits on ; && || | & newline outside quotes, tokenizes, and
// recurses into `sh -c "..."` style wrappers.
func segments(cmd string, depth int) [][]string {
	var out [][]string
	var cur []string
	var tok strings.Builder
	inTok := false
	var quote rune
	flushTok := func() {
		if inTok {
			cur = append(cur, tok.String())
			tok.Reset()
			inTok = false
		}
	}
	flushSeg := func() {
		flushTok()
		if len(cur) > 0 {
			out = append(out, cur)
			cur = nil
		}
	}
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' && i+1 < len(rs) && rs[i+1] == '"' {
				i++
				tok.WriteRune(rs[i])
			} else {
				tok.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inTok = r, true
		case r == '\\' && i+1 < len(rs) && rs[i+1] != '\\' && rs[i+1] != '\n' && !isWinPathChar(rs, i):
			i++
			tok.WriteRune(rs[i])
			inTok = true
		case r == ' ' || r == '\t' || r == '\r':
			flushTok()
		case r == ';' || r == '\n' || r == '|' || r == '&' || r == '(' || r == ')':
			flushSeg()
		default:
			tok.WriteRune(r)
			inTok = true
		}
	}
	flushSeg()
	if depth < 2 {
		var extra [][]string
		for _, seg := range out {
			if inner, ok := wrapped(seg); ok {
				extra = append(extra, segments(inner, depth+1)...)
			}
		}
		out = append(out, extra...)
	}
	return out
}

// isWinPathChar: treat backslash as literal path separator (C:\x, .\x) rather than an escape.
func isWinPathChar(rs []rune, i int) bool {
	return i > 0 && (rs[i-1] == ':' || rs[i-1] == '.' || isWordRune(rs[i-1]))
}
func isWordRune(r rune) bool {
	return r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func wrapped(seg []string) (string, bool) {
	if len(seg) < 3 {
		return "", false
	}
	switch bin(seg[0]) {
	case "sh", "bash", "zsh", "dash", "fish":
		for i := 1; i < len(seg)-1; i++ {
			if seg[i] == "-c" || seg[i] == "-lc" || seg[i] == "-ic" {
				return seg[i+1], true
			}
		}
	case "cmd":
		for i := 1; i < len(seg)-1; i++ {
			if strings.EqualFold(seg[i], "/c") {
				return strings.Join(seg[i+1:], " "), true
			}
		}
	case "powershell", "pwsh":
		for i := 1; i < len(seg)-1; i++ {
			if l := strings.ToLower(seg[i]); l == "-command" || l == "-c" {
				return strings.Join(seg[i+1:], " "), true
			}
		}
	}
	return "", false
}

// bin normalizes an executable token: basename, lowercase, no .exe/.cmd/.bat/.ps1.
func bin(t string) string {
	t = strings.ReplaceAll(t, `\`, "/")
	t = strings.ToLower(path.Base(t))
	for _, s := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		t = strings.TrimSuffix(t, s)
	}
	return t
}

func isAssign(t string) bool {
	i := strings.Index(t, "=")
	if i <= 0 {
		return false
	}
	for _, r := range t[:i] {
		if !isWordRune(r) {
			return false
		}
	}
	return true
}

// wrappers are prefix launchers skipped to reach the real command.
var wrappers = map[string]bool{
	"sudo": true, "time": true, "env": true, "command": true, "nice": true, "nohup": true,
	"exec": true, "npx": true, "bunx": true, "pnpx": true, "dlx": true, "timeout": true,
	"stdbuf": true, "caffeinate": true, "xvfb-run": true, "doppler": false,
}

func classifySegment(seg []string) string {
	// strip env assignments and launcher prefixes
	for len(seg) > 0 {
		t := seg[0]
		if isAssign(t) {
			seg = seg[1:]
			continue
		}
		b := bin(t)
		if wrappers[b] {
			seg = seg[1:]
			// skip option-like args and (for timeout) a duration
			for len(seg) > 0 && (strings.HasPrefix(seg[0], "-") || isAssign(seg[0]) || (b == "timeout" && isDuration(seg[0]))) {
				seg = seg[1:]
			}
			continue
		}
		// runners: uv run X, poetry run X, pipenv run X, bundle exec X, pdm run, rye run, hatch run
		if len(seg) >= 3 && ((b == "uv" || b == "poetry" || b == "pipenv" || b == "pdm" || b == "rye" || b == "hatch") && seg[1] == "run") {
			seg = seg[2:]
			for len(seg) > 0 && strings.HasPrefix(seg[0], "-") {
				seg = seg[1:]
			}
			continue
		}
		if len(seg) >= 3 && b == "bundle" && seg[1] == "exec" {
			seg = seg[2:]
			continue
		}
		break
	}
	if len(seg) == 0 {
		return ""
	}
	b := bin(seg[0])
	args := seg[1:]
	switch b {
	case "npm", "pnpm", "yarn", "bun", "bunx", "deno":
		return classifyJS(b, args)
	case "jest", "vitest", "mocha", "ava", "tap", "karma", "pytest", "py.test", "tox", "nox", "rspec",
		"phpunit", "ctest", "bats", "playwright", "cypress", "behave", "nosetests", "trial":
		if (b == "playwright" || b == "cypress") && !hasArg(args, "test", "run") {
			return ""
		}
		return event.RunTest
	case "python", "python3", "py", "pypy", "pypy3":
		return classifyPython(args)
	case "cargo":
		return classifyCargo(args)
	case "go":
		return classifyGo(args)
	case "make", "gmake", "nmake", "ninja", "just", "task", "mage":
		if hasArg(args, "test", "tests", "check", "spec", "ci") {
			return event.RunTest
		}
		if hasArg(args, "run", "dev", "serve", "start", "watch") {
			return event.RunDev
		}
		if len(args) > 0 && (args[0] == "install" || args[0] == "clean" || args[0] == "lint" || args[0] == "fmt") {
			return ""
		}
		return event.RunBuild
	case "cmake", "meson", "bazel", "bazelisk", "buck2", "xcodebuild", "msbuild", "scons", "swift", "zig", "flutter", "dart", "deno-compile":
		return classifyBuildTool(b, args)
	case "gradle", "gradlew", "mvn", "mvnw", "sbt", "ant", "dotnet", "mix", "rake", "lein", "stack", "cabal", "dune", "nx", "turbo":
		return classifyJVMish(b, args)
	case "tsc":
		if hasArg(args, "--watch", "-w") {
			return event.RunDev
		}
		return event.RunBuild
	case "vite":
		if hasArg(args, "build") {
			return event.RunBuild
		}
		return event.RunDev
	case "next", "nuxt", "astro", "remix", "svelte-kit", "gatsby", "ng", "expo", "hugo", "jekyll", "eleventy", "storybook", "parcel", "webpack", "webpack-dev-server", "rollup", "esbuild", "turbopack":
		return classifyFramework(b, args)
	case "nodemon", "uvicorn", "gunicorn", "hypercorn", "daphne", "flask", "streamlit", "air", "reflex", "wrangler", "supabase", "netlify", "firebase":
		return classifyServer(b, args)
	case "docker", "docker-compose", "podman":
		return classifyDocker(b, args)
	case "gcc", "g++", "cc", "c++", "clang", "clang++", "rustc", "javac", "nvcc", "tcc", "go-build", "ghc", "nim", "crystal", "kotlinc", "scalac", "fpc":
		return event.RunBuild
	case "node", "nodejs", "tsx", "ts-node", "ts-node-esm", "bun-run", "ruby", "php", "perl", "lua", "luajit", "julia", "rscript", "java", "kotlin", "scala", "elixir", "escript", "racket", "guile", "runghc", "sbcl", "ocaml", "swipl":
		return classifyInterp(b, args)
	case "rails":
		if hasArg(args, "test", "t") {
			return event.RunTest
		}
		if hasArg(args, "server", "s", "console", "c") {
			return event.RunDev
		}
		return ""
	case "ruff", "eslint", "prettier", "black", "mypy", "pyright", "golangci-lint", "clippy-driver", "shellcheck", "git", "ls", "cat", "grep", "rg", "find", "cd", "echo", "mkdir", "rm", "cp", "mv", "sed", "awk", "head", "tail", "curl", "wget":
		return ""
	}
	// ./script, ./a.out, path/to/binary built from the project
	t := seg[0]
	if strings.HasSuffix(t, ".py") || strings.HasSuffix(t, ".rb") {
		return event.RunExec
	}
	if strings.HasPrefix(t, "./") || strings.HasPrefix(t, ".\\") || strings.HasPrefix(t, "target/") || strings.HasPrefix(t, "bin/") || strings.HasPrefix(t, "build/") {
		switch bin(t) {
		case "gradlew", "mvnw":
			return classifyJVMish(bin(t), args)
		}
		return event.RunExec
	}
	return ""
}

func isDuration(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && r != '.' && r != 's' && r != 'm' && r != 'h' && r != 'd' {
			return false
		}
	}
	return true
}

func hasArg(args []string, want ...string) bool {
	for _, a := range args {
		for _, w := range want {
			if a == w {
				return true
			}
		}
	}
	return false
}

// firstNonFlag returns the first arg not starting with '-'.
func firstNonFlag(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, args[i+1:]
		}
	}
	return "", nil
}

func classifyScriptName(name string) string {
	n := strings.ToLower(name)
	base := n
	if i := strings.IndexAny(n, ":"); i >= 0 {
		base = n[:i]
	}
	switch base {
	case "test", "tests", "t", "e2e", "spec", "jest", "vitest", "coverage", "check", "ci", "test-ci", "unit", "integration", "cypress", "playwright", "pytest":
		return event.RunTest
	case "build", "compile", "bundle", "dist", "package", "prod", "generate", "export", "typecheck", "tsc":
		return event.RunBuild
	case "dev", "start", "serve", "watch", "storybook", "preview", "develop", "server", "run", "web", "electron", "tauri", "docs":
		return event.RunDev
	}
	if strings.HasPrefix(base, "test") {
		return event.RunTest
	}
	if strings.HasPrefix(base, "build") {
		return event.RunBuild
	}
	if strings.HasPrefix(base, "dev") || strings.HasPrefix(base, "start") {
		return event.RunDev
	}
	return ""
}

func classifyJS(b string, args []string) string {
	sub, rest := firstNonFlag(args)
	switch b {
	case "deno":
		switch sub {
		case "test":
			return event.RunTest
		case "run":
			if hasArg(args, "--watch") {
				return event.RunDev
			}
			return event.RunExec
		case "task":
			s, _ := firstNonFlag(rest)
			return classifyScriptName(s)
		case "compile":
			return event.RunBuild
		}
		return ""
	case "bunx":
		return classifyJS("bun", append([]string{"x"}, args...))
	}
	switch sub {
	case "test", "t", "tst":
		return event.RunTest
	case "start", "dev", "serve":
		if b == "bun" && sub == "dev" {
			return event.RunDev
		}
		return event.RunDev
	case "build":
		return event.RunBuild
	case "run", "run-script", "exec", "x", "dlx":
		if sub == "x" || sub == "dlx" || sub == "exec" {
			// bun x / pnpm dlx / npm exec <tool> ...
			s, r := firstNonFlag(rest)
			if s == "" {
				return ""
			}
			return classifySegment(append([]string{s}, r...))
		}
		s, _ := firstNonFlag(rest)
		if b == "bun" && (strings.HasSuffix(s, ".js") || strings.HasSuffix(s, ".ts") || strings.HasSuffix(s, ".tsx") || strings.HasSuffix(s, ".mjs")) {
			return event.RunExec
		}
		return classifyScriptName(s)
	case "ci", "install", "i", "add", "remove", "update", "upgrade", "init", "publish", "link", "audit", "outdated", "config", "cache", "login", "pack", "version", "why", "ls", "list", "info", "create":
		return ""
	}
	if b == "yarn" || b == "pnpm" {
		// yarn <script> / pnpm <script>
		if k := classifyScriptName(sub); k != "" {
			return k
		}
	}
	if b == "bun" && (strings.HasSuffix(sub, ".js") || strings.HasSuffix(sub, ".ts") || strings.HasSuffix(sub, ".tsx") || strings.HasSuffix(sub, ".mjs")) {
		return event.RunExec
	}
	if b == "pnpm" && (sub == "exec" || sub == "dlx") {
		return ""
	}
	return ""
}

func classifyPython(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--version" || a == "-V" || a == "-h":
			return ""
		case a == "-m":
			if i+1 >= len(args) {
				return ""
			}
			switch args[i+1] {
			case "pytest", "unittest", "nose", "nose2", "tox", "doctest":
				return event.RunTest
			case "http.server":
				return event.RunDev
			case "flask", "uvicorn", "gunicorn", "streamlit", "django", "hypercorn", "daphne":
				if hasArg(args[i+2:], "run", "runserver") || args[i+1] != "django" {
					return event.RunDev
				}
				return ""
			case "build", "compileall", "py_compile", "nuitka", "PyInstaller", "pyinstaller", "setuptools":
				return event.RunBuild
			case "pip", "venv", "ensurepip", "virtualenv", "black", "ruff", "mypy", "pylint", "flake8", "json.tool", "site", "pip-compile", "pipx", "uv":
				return ""
			}
			return event.RunExec
		case strings.HasPrefix(a, "-"):
			continue
		default:
			if strings.HasSuffix(a, "manage.py") {
				sub, _ := firstNonFlag(args[i+1:])
				switch sub {
				case "test":
					return event.RunTest
				case "runserver", "runserver_plus":
					return event.RunDev
				}
				return ""
			}
			if a == "setup.py" {
				if hasArg(args[i+1:], "test") {
					return event.RunTest
				}
				if hasArg(args[i+1:], "build", "bdist_wheel", "sdist", "build_ext") {
					return event.RunBuild
				}
				return ""
			}
			return event.RunExec
		}
	}
	return "" // bare REPL
}

func classifyCargo(args []string) string {
	sub, rest := firstNonFlag(args)
	switch sub {
	case "test", "nextest", "t", "miri", "tarpaulin", "llvm-cov", "insta":
		return event.RunTest
	case "build", "b", "check", "c", "clippy", "doc", "bench", "install", "package", "publish", "zigbuild", "xwin":
		if sub == "install" || sub == "publish" || sub == "package" {
			return event.RunBuild
		}
		return event.RunBuild
	case "run", "r":
		return event.RunExec
	case "watch", "leptos", "tauri", "dioxus", "trunk":
		if sub == "watch" {
			if hasArg(rest, "-x") {
				for i, a := range rest {
					if a == "-x" && i+1 < len(rest) && strings.HasPrefix(rest[i+1], "test") {
						return event.RunTest
					}
				}
			}
			return event.RunDev
		}
		return event.RunDev
	}
	return ""
}

func classifyGo(args []string) string {
	sub, _ := firstNonFlag(args)
	switch sub {
	case "test":
		return event.RunTest
	case "build", "install", "generate", "vet":
		if sub == "vet" {
			return event.RunBuild
		}
		return event.RunBuild
	case "run":
		return event.RunExec
	}
	return ""
}

func classifyBuildTool(b string, args []string) string {
	sub, rest := firstNonFlag(args)
	switch b {
	case "swift":
		switch sub {
		case "test":
			return event.RunTest
		case "build":
			return event.RunBuild
		case "run":
			return event.RunExec
		}
	case "zig":
		switch sub {
		case "test":
			return event.RunTest
		case "build", "build-exe", "build-lib", "cc", "c++":
			if hasArg(rest, "test") {
				return event.RunTest
			}
			return event.RunBuild
		case "run":
			return event.RunExec
		}
	case "flutter":
		switch sub {
		case "test":
			return event.RunTest
		case "build":
			return event.RunBuild
		case "run":
			return event.RunDev
		}
	case "dart":
		switch sub {
		case "test":
			return event.RunTest
		case "compile":
			return event.RunBuild
		case "run":
			return event.RunExec
		}
	case "bazel", "bazelisk", "buck2":
		switch sub {
		case "test":
			return event.RunTest
		case "build":
			return event.RunBuild
		case "run":
			return event.RunExec
		}
	case "xcodebuild":
		if hasArg(args, "test", "test-without-building") {
			return event.RunTest
		}
		return event.RunBuild
	default: // cmake, meson, msbuild, scons
		if sub == "test" || hasArg(args, "test") {
			return event.RunTest
		}
		return event.RunBuild
	}
	return ""
}

func classifyJVMish(b string, args []string) string {
	sub, _ := firstNonFlag(args)
	switch b {
	case "gradle", "gradlew":
		if hasArg(args, "-t", "--continuous") {
			return event.RunDev
		}
		for _, a := range args {
			l := strings.ToLower(a)
			base := l[strings.LastIndex(l, ":")+1:]
			switch {
			case base == "test" || base == "check" || strings.HasPrefix(base, "test") || strings.HasSuffix(base, "test") || base == "connectedcheck":
				return event.RunTest
			case base == "run" || base == "bootrun" || base == "runclient" || base == "quarkusdev" || base == "installdebug":
				return event.RunDev
			case strings.HasPrefix(base, "build") || strings.HasPrefix(base, "assemble") || base == "jar" || base == "compilejava" || base == "compilekotlin" || strings.HasPrefix(base, "bundle") || base == "shadowjar" || base == "classes" || base == "install":
				return event.RunBuild
			}
		}
		return ""
	case "mvn", "mvnw":
		for _, a := range args {
			switch a {
			case "test", "verify", "integration-test", "surefire:test", "failsafe:integration-test":
				return event.RunTest
			case "spring-boot:run", "quarkus:dev", "jetty:run", "tomcat7:run", "exec:java":
				return event.RunDev
			case "compile", "package", "install", "clean", "test-compile", "assembly:single":
				return event.RunBuild
			}
		}
		return ""
	case "sbt":
		for _, a := range args {
			switch a {
			case "test", "testOnly", "it:test":
				return event.RunTest
			case "run", "~run", "reStart":
				return event.RunDev
			case "compile", "package", "assembly", "stage":
				return event.RunBuild
			}
		}
		return ""
	case "dotnet":
		switch sub {
		case "test":
			return event.RunTest
		case "build", "publish", "pack", "msbuild":
			return event.RunBuild
		case "run":
			return event.RunExec
		case "watch":
			return event.RunDev
		}
		return ""
	case "mix":
		switch sub {
		case "test":
			return event.RunTest
		case "compile", "release", "escript.build":
			return event.RunBuild
		case "phx.server", "run":
			return event.RunDev
		}
		return ""
	case "rake":
		switch sub {
		case "test", "spec":
			return event.RunTest
		case "":
			return event.RunBuild
		}
		return classifyScriptName(sub)
	case "ant":
		if sub == "test" || sub == "junit" {
			return event.RunTest
		}
		return event.RunBuild
	case "lein":
		switch sub {
		case "test":
			return event.RunTest
		case "run", "repl":
			return event.RunExec
		case "uberjar", "jar", "compile":
			return event.RunBuild
		}
		return ""
	case "stack", "cabal":
		switch sub {
		case "test":
			return event.RunTest
		case "build", "install":
			return event.RunBuild
		case "run", "exec", "repl":
			return event.RunExec
		}
		return ""
	case "dune":
		switch sub {
		case "test", "runtest":
			return event.RunTest
		case "build":
			return event.RunBuild
		case "exec":
			return event.RunExec
		}
		return ""
	case "nx", "turbo":
		s, rest := firstNonFlag(args)
		if s == "run" || s == "run-many" {
			s, _ = firstNonFlag(rest)
			if i := strings.Index(s, ":"); i >= 0 {
				s = s[i+1:]
			}
		}
		return classifyScriptName(s)
	}
	return ""
}

func classifyFramework(b string, args []string) string {
	sub, _ := firstNonFlag(args)
	switch b {
	case "webpack", "rollup", "esbuild", "parcel":
		if hasArg(args, "--watch", "-w", "serve", "watch") {
			return event.RunDev
		}
		return event.RunBuild
	case "webpack-dev-server":
		return event.RunDev
	case "turbopack", "storybook", "eleventy":
		if sub == "build" {
			return event.RunBuild
		}
		return event.RunDev
	}
	switch sub {
	case "build", "export", "generate", "static":
		return event.RunBuild
	case "dev", "start", "serve", "server", "preview", "develop", "run", "s":
		return event.RunDev
	case "test":
		return event.RunTest
	}
	if sub == "" && (b == "hugo") {
		return event.RunBuild
	}
	return ""
}

func classifyServer(b string, args []string) string {
	if b == "flask" || b == "streamlit" {
		if hasArg(args, "run") {
			return event.RunDev
		}
		return ""
	}
	if b == "wrangler" || b == "supabase" || b == "netlify" || b == "firebase" {
		sub, _ := firstNonFlag(args)
		switch sub {
		case "dev", "start", "serve", "emulators:start":
			return event.RunDev
		case "build":
			return event.RunBuild
		}
		return ""
	}
	return event.RunDev
}

func classifyDocker(b string, args []string) string {
	sub, rest := firstNonFlag(args)
	if b == "docker" && sub == "compose" {
		sub, _ = firstNonFlag(rest)
	}
	switch sub {
	case "build", "buildx", "bake":
		return event.RunBuild
	case "up", "run", "start", "watch":
		return event.RunDev
	}
	return ""
}

func classifyInterp(b string, args []string) string {
	if len(args) == 0 {
		return ""
	}
	switch b {
	case "node", "nodejs":
		if hasArg(args, "--test") {
			return event.RunTest
		}
		if hasArg(args, "--watch") {
			return event.RunDev
		}
	case "java":
		if hasArg(args, "-version", "--version") {
			return ""
		}
	case "ruby":
		if hasArg(args, "-v", "--version") {
			return ""
		}
	}
	for _, a := range args {
		if a == "-e" || a == "-c" || a == "-v" || a == "--version" || a == "-p" || a == "--eval" || a == "-r" && b == "node" {
			if a == "-e" || a == "-c" || a == "-p" || a == "--eval" || a == "-v" || a == "--version" {
				return ""
			}
		}
	}
	return event.RunExec
}
