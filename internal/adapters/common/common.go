// Package common holds helpers shared by the harness adapter subpackages. It
// must not import package adapters (which imports the subpackages via
// registry.go), so it declares its own structural Sink/Adapter types.
package common

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"time"

	"seatime/pkg/event"
)

// Sink is structurally identical to adapters.Sink.
type Sink = interface{ Emit(e event.Event) error }

// Adapter mirrors adapters.Adapter but with the structural Sink; registry.go
// wraps these into adapters.Adapter values.
type Adapter interface {
	Name() string
	Detect() bool
	Installed() (bool, error)
	Install(seatimeBin string) error
	Uninstall() error
	HandleHook(hookEvent string, stdin []byte, sink Sink) (stdout []byte, err error)
}

// Watcher mirrors adapters.Watcher.
type Watcher interface {
	Watch(ctx context.Context, stateDir string, sink Sink) error
}

// Home returns the user's home directory, honoring SEATIME_FAKE_HOME so tests and
// dry runs never touch the real harness config.
func Home() string {
	if h := os.Getenv("SEATIME_FAKE_HOME"); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// Safe runs fn, converting panics into errors (hooks must never crash).
func Safe(fn func() ([]byte, error)) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return fn()
}

// WriteFile writes atomically (temp + rename), creating parent dirs.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".seatime-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	tmp.Close()
	os.Chmod(name, perm)
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// releaseName matches a release binary run under its download name.
var releaseName = regexp.MustCompile(`^seatime-(darwin|linux|windows)-(amd64|arm64|386)$`)

var safeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./\\-]+$`)

// HookCommand builds the command line `<bin> hook <name> <event>` with the
// binary path quoted when needed. Quoting uses double quotes (valid in sh,
// cmd.exe and — when invoked via `&` — PowerShell).
func HookCommand(bin, name, event string) string {
	return Quote(bin) + " hook " + name + " " + event
}

// Quote double-quotes s if it contains characters outside a safe set.
func Quote(s string) string {
	if safeWord.MatchString(s) && !strings.Contains(s, " ") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// NeedsQuote reports whether HookCommand would quote bin.
func NeedsQuote(bin string) bool { return Quote(bin) != bin }

// IsWindows is a var for testing.
var IsWindows = runtime.GOOS == "windows"

// IsSeatimeCommand reports whether cmd is a SeaTime-installed hook command for the
// adapter: `[& ]<path to seatime[.exe]> hook <name> <event>`.
func IsSeatimeCommand(cmd, name string) bool {
	c := strings.TrimSpace(cmd)
	c = strings.TrimPrefix(c, "& ")
	var bin, rest string
	if strings.HasPrefix(c, `"`) {
		end := strings.Index(c[1:], `"`)
		if end < 0 {
			return false
		}
		bin, rest = c[1:1+end], c[end+2:]
	} else {
		i := strings.IndexAny(c, " \t")
		if i < 0 {
			return false
		}
		bin, rest = c[:i], c[i:]
	}
	base := strings.ToLower(bin)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".exe")
	// "seatime", or a release file run under its download name (seatime-darwin-arm64)
	if base != "seatime" && !releaseName.MatchString(base) {
		return false
	}
	f := strings.Fields(rest)
	return len(f) >= 2 && f[0] == "hook" && f[1] == name
}

// ---- JSON config editing (structure-preserving: unknown keys kept) ----

// ReadJSONObject reads a JSON object file; missing/empty file yields an empty map.
func ReadJSONObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(b)) == "" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: not valid JSON (refusing to edit): %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s: extra content after the JSON object (refusing to edit)", path)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// WriteJSONObject writes m as indented JSON, unless the file already holds
// the same JSON (no write when nothing changed). Before seatime's first edit of
// a file it keeps one backup of the original (BackupOnce), and an existing
// file keeps its permissions.
func WriteJSONObject(path string, m map[string]any, perm os.FileMode) error {
	if cur, err := os.ReadFile(path); err == nil && sameJSON(cur, m) {
		return nil
	}
	if err := BackupOnce(path); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	b, err := encodeJSON(m)
	if err != nil {
		return err
	}
	return WriteFile(path, b, perm)
}

func encodeJSON(m map[string]any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

// sameJSON reports whether raw parses to the same JSON value as m.
func sameJSON(raw []byte, m map[string]any) bool {
	var a any
	if strings.TrimSpace(string(raw)) == "" {
		a = map[string]any{}
	} else {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if dec.Decode(&a) != nil || dec.More() {
			return false
		}
	}
	mb, err := json.Marshal(m)
	if err != nil {
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(mb)))
	dec.UseNumber()
	var b any
	if dec.Decode(&b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// BackupPath is where SeaTime keeps the original of a config file it edits;
// AbsentPath marks that the file did not exist before SeaTime created it.
func BackupPath(path string) string { return path + ".seatime-bak" }
func AbsentPath(path string) string { return path + ".seatime-absent" }

// BackupOnce records the file's state before seatime's first edit: a copy of
// it, or a marker that it did not exist. Later edits keep that one backup.
func BackupOnce(path string) error {
	for _, p := range []string{BackupPath(path), AbsentPath(path)} {
		if _, err := os.Stat(p); err == nil {
			return nil
		}
	}
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(AbsentPath(path), nil, 0o600)
	}
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return WriteFile(BackupPath(path), b, st.Mode().Perm())
}

// restoreOriginal finishes an uninstall: when removing seatime's entries left
// exactly the original content, the original bytes come back (formatting,
// key order and permissions included) and the backup goes; a file SeaTime
// created is removed again. If the user changed the file since, the edited
// content (minus SeaTime) is written and the stale backup is dropped.
func restoreOriginal(path string, root map[string]any, perm os.FileMode, added ...string) error {
	if _, err := os.Stat(AbsentPath(path)); err == nil {
		rest := len(root)
		for _, k := range added { // top-level keys SeaTime itself added to a file it created
			if _, ok := root[k]; ok {
				rest--
			}
		}
		if rest == 0 {
			os.Remove(AbsentPath(path))
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
	} else if b, err := os.ReadFile(BackupPath(path)); err == nil && sameJSON(b, root) {
		mode := perm
		if st, err := os.Stat(BackupPath(path)); err == nil {
			mode = st.Mode().Perm()
		}
		if err := WriteFile(path, b, mode); err != nil {
			return err
		}
		return os.Remove(BackupPath(path))
	}
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	b, err := encodeJSON(root)
	if err != nil {
		return err
	}
	if err := WriteFile(path, b, perm); err != nil {
		return err
	}
	os.Remove(BackupPath(path))
	os.Remove(AbsentPath(path))
	return nil
}

// RestoreOriginal is restoreOriginal for adapters with their own config shape.
func RestoreOriginal(path string, root map[string]any, perm os.FileMode) error {
	return restoreOriginal(path, root, perm)
}

// HookSpec is one hook entry to install.
type HookSpec struct {
	Event   string         // harness event name (also the `seatime hook` arg)
	Matcher string         // matcher-group formats only; "" omits
	Extra   map[string]any // extra keys merged into the handler object
}

// groupsFormat selects the config shape.
type Format int

const (
	// MatcherGroups: hooks[event] = [{matcher?, hooks:[{type,command,...}]}]
	// (Claude Code, Codex, Gemini).
	MatcherGroups Format = iota
	// Flat: hooks[event] = [{command,...}] (Cursor).
	Flat
)

func isSeatime(h any, name string) bool {
	m, ok := h.(map[string]any)
	if !ok {
		return false
	}
	c, _ := m["command"].(string)
	return IsSeatimeCommand(c, name)
}

// stripSeatime removes SeaTime entries from hooks[event]; returns the pruned list.
func stripSeatime(list []any, name string, f Format) []any {
	var out []any
	for _, it := range list {
		if f == Flat {
			if !isSeatime(it, name) {
				out = append(out, it)
			}
			continue
		}
		g, ok := it.(map[string]any)
		if !ok {
			out = append(out, it)
			continue
		}
		inner, ok := g["hooks"].([]any)
		if !ok {
			out = append(out, it)
			continue
		}
		var keep []any
		for _, h := range inner {
			if !isSeatime(h, name) {
				keep = append(keep, h)
			}
		}
		if len(keep) == len(inner) {
			out = append(out, it)
		} else if len(keep) > 0 {
			g["hooks"] = keep
			out = append(out, g)
		}
	}
	return out
}

// InstallHooks idempotently installs specs into the JSON config at path.
// cmdFor renders the command string per event. topLevel keys are set only
// when absent (e.g. Cursor's "version").
func InstallHooks(path, name string, f Format, specs []HookSpec, cmdFor func(event string) string, topLevel map[string]any) error {
	root, err := ReadJSONObject(path)
	if err != nil {
		return err
	}
	hooks, _ := root["hooks"].(map[string]any)
	if root["hooks"] != nil && hooks == nil {
		return fmt.Errorf("%s: unexpected \"hooks\" shape; refusing to edit", path)
	}
	if hooks == nil {
		hooks = map[string]any{}
	}
	// remove stale SeaTime entries from every event (handles changed event sets).
	for ev, v := range hooks {
		if l, ok := v.([]any); ok {
			if n := stripSeatime(l, name, f); len(n) == 0 {
				delete(hooks, ev)
			} else {
				hooks[ev] = n
			}
		}
	}
	for _, s := range specs {
		h := map[string]any{"command": cmdFor(s.Event)}
		if f == MatcherGroups {
			h["type"] = "command"
		} else if _, ok := s.Extra["type"]; !ok {
			h["type"] = "command"
		}
		for k, v := range s.Extra {
			if v == nil { // nil = make sure the key is absent
				delete(h, k)
				continue
			}
			h[k] = v
		}
		var entry any = h
		if f == MatcherGroups {
			g := map[string]any{"hooks": []any{h}}
			if s.Matcher != "" {
				g["matcher"] = s.Matcher
			}
			entry = g
		}
		l, _ := hooks[s.Event].([]any)
		hooks[s.Event] = append(l, entry)
	}
	root["hooks"] = hooks
	for k, v := range topLevel {
		if _, ok := root[k]; !ok {
			root[k] = v
		}
	}
	return WriteJSONObject(path, root, 0o600)
}

// UninstallHooks removes exactly seatime's entries, pruning emptied containers.
// A missing file is a no-op and is not backed up/rewritten.
func UninstallHooks(path, name string, f Format) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	root, err := ReadJSONObject(path)
	if err != nil {
		return err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	changed := false
	for ev, v := range hooks {
		l, ok := v.([]any)
		if !ok {
			continue
		}
		n := stripSeatime(l, name, f)
		if len(n) != len(l) || !sameLen(n, l) {
			changed = true
		}
		if len(n) == 0 {
			delete(hooks, ev)
		} else {
			hooks[ev] = n
		}
	}
	if !changed {
		return nil
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
	var added []string
	if f == Flat {
		added = []string{"version"} // Cursor's required top-level key
	}
	return restoreOriginal(path, root, 0o600, added...)
}

func sameLen(a, b []any) bool { return len(a) == len(b) }

// HooksInstalled reports whether any seatime hook entry exists in path.
func HooksInstalled(path, name string, f Format) (bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}
	root, err := ReadJSONObject(path)
	if err != nil {
		return false, err
	}
	hooks, _ := root["hooks"].(map[string]any)
	for _, v := range hooks {
		l, _ := v.([]any)
		if len(stripSeatime(deepCopy(l), name, f)) != len(l) || groupsChanged(l, name, f) {
			return true, nil
		}
	}
	return false, nil
}

func deepCopy(l []any) []any {
	b, _ := json.Marshal(l)
	var out []any
	json.Unmarshal(b, &out)
	return out
}

func groupsChanged(l []any, name string, f Format) bool {
	for _, it := range l {
		if f == Flat {
			if isSeatime(it, name) {
				return true
			}
			continue
		}
		g, _ := it.(map[string]any)
		inner, _ := g["hooks"].([]any)
		for _, h := range inner {
			if isSeatime(h, name) {
				return true
			}
		}
	}
	return false
}

// ---- payload helpers ----

// Obj decodes stdin into a generic map (UseNumber off; numbers as float64).
func Obj(stdin []byte) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(stdin, &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// Str returns the first non-empty string value among keys.
func Str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// Map returns m[key] as a map (nil if absent).
func Map(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

// ParseTime parses RFC3339 timestamps (and unix seconds/ms numbers); zero on failure.
func ParseTime(v any) time.Time {
	switch t := v.(type) {
	case string:
		for _, l := range []string{time.RFC3339Nano, time.RFC3339} {
			if x, err := time.Parse(l, t); err == nil {
				return x.UTC()
			}
		}
	case float64:
		switch {
		case t > 1e12:
			return time.UnixMilli(int64(t)).UTC()
		case t > 1e9:
			return time.Unix(int64(t), 0).UTC()
		}
	}
	return time.Time{}
}

var exitRe = regexp.MustCompile(`(?i)(?:exit(?:ed)?(?: with)? ?(?:code|status)?[: ]+)(-?\d+)`)

// ParseExitCode finds "Exit code N" style text; ok=false if none.
func ParseExitCode(s string) (int, bool) {
	m := exitRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

// IntPtr returns &n.
func IntPtr(n int) *int { return &n }

// EmitRun emits a run event if command classifies as a project run.
// (classification is passed in to keep this package free of cycles.)
// output is the command's captured output; nothing of it is kept.
func EmitRun(sink Sink, base event.Event, command, kind string, exit *int, output string) error {
	e := base
	e.Kind = event.Run
	e.Command = command
	e.RunKind = kind
	e.ExitCode = exit
	e.Tool = ""
	return sink.Emit(e)
}

// OutputText pulls best-effort command output text out of a hook payload,
// across the field names the supported harnesses use.
func OutputText(p map[string]any) string {
	var parts []string
	add := func(m map[string]any) {
		for _, k := range []string{"stderr", "stdout", "output", "error", "llmContent", "returnDisplay", "content"} {
			if s, ok := m[k].(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
	}
	add(p)
	if r, ok := p["tool_response"].(map[string]any); ok {
		add(r)
	} else if s, ok := p["tool_response"].(string); ok {
		parts = append(parts, s)
	}
	if md, ok := p["metadata"].(map[string]any); ok {
		add(md)
	}
	return strings.Join(parts, "\n")
}
