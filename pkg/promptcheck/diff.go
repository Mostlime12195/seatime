package promptcheck

import (
	"fmt"
	"sort"
	"strings"
)

// NoChange is the diff text of a turn that changed nothing.
const NoChange = "(no change)"

// Snapshot is the project's uncommitted state at one moment: `git diff
// HEAD` cut into one section per file, and the untracked files with their
// first lines. The device takes one when a prompt is sent and one when the
// turn ends; TurnDiff compares them.
type Snapshot struct {
	Files     map[string]string // path -> its `git diff` section
	Untracked map[string]string // path -> "N lines" header plus the first lines
}

// ParseDiff cuts `git diff` output into sections by file.
func ParseDiff(out string) map[string]string {
	files := map[string]string{}
	var name string
	var b strings.Builder
	flush := func() {
		if name != "" {
			files[name] = b.String()
		}
		b.Reset()
	}
	for _, l := range strings.SplitAfter(out, "\n") {
		if strings.HasPrefix(l, "diff --git ") {
			flush()
			name = strings.TrimSpace(l[len("diff --git "):])
			if i := strings.Index(name, " b/"); i >= 0 {
				name = name[i+3:]
			}
		}
		if name != "" {
			b.WriteString(l)
		}
	}
	flush()
	return files
}

// TurnDiff is the excerpt of what a turn changed: the diff section of each
// tracked file whose section differs from the start of the turn (a file
// put back as it was shows as reverted), then each untracked file that is
// new or different, in path order, cut to MaxDiff bytes. It reports
// whether it was cut.
func TurnDiff(start, end Snapshot) (string, bool) {
	var b strings.Builder
	for _, p := range sortedKeys(union(start.Files, end.Files)) {
		s, e := start.Files[p], end.Files[p]
		switch {
		case s == e:
		case e == "":
			fmt.Fprintf(&b, "%s: changes from earlier in the session were undone\n", p)
		default:
			b.WriteString(e)
			if !strings.HasSuffix(e, "\n") {
				b.WriteString("\n")
			}
		}
	}
	for _, p := range sortedKeys(end.Untracked) {
		if start.Untracked[p] == end.Untracked[p] {
			continue
		}
		fmt.Fprintf(&b, "new file, not yet added to git: %s, %s\n", p, end.Untracked[p])
	}
	for _, p := range sortedKeys(start.Untracked) {
		if _, ok := end.Untracked[p]; !ok {
			if _, tracked := end.Files[p]; !tracked {
				fmt.Fprintf(&b, "%s: removed\n", p)
			}
		}
	}
	if b.Len() == 0 {
		return NoChange, false
	}
	return Cut(b.String(), MaxDiff)
}

func union(a, b map[string]string) map[string]string {
	u := map[string]string{}
	for k := range a {
		u[k] = ""
	}
	for k := range b {
		u[k] = ""
	}
	return u
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// DiffLines counts the added and removed lines in the `git diff` sections
// of an excerpt (not the headers, and not untracked files, which git's
// line counts leave out too).
func DiffLines(excerpt string) (add, del int) {
	in := false
	for _, l := range strings.Split(excerpt, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			in = true
		case !in:
		case strings.HasPrefix(l, "+++ "), strings.HasPrefix(l, "--- "):
		case strings.HasPrefix(l, "+"):
			add++
		case strings.HasPrefix(l, "-"):
			del++
		case l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "@@") && !strings.HasPrefix(l, "\\") &&
			!strings.HasPrefix(l, "index ") && !strings.HasPrefix(l, "new file mode") && !strings.HasPrefix(l, "deleted file mode") &&
			!strings.HasPrefix(l, "old mode") && !strings.HasPrefix(l, "new mode") && !strings.HasPrefix(l, "similarity") &&
			!strings.HasPrefix(l, "rename ") && !strings.HasPrefix(l, "Binary files"):
			in = false // a line of our own between sections
		}
	}
	return add, del
}

// Slack is how far an excerpt's line counts may go past git's logged
// counts before the check request is refused (the excerpt is cut, and a
// file can change between the two reads).
const Slack = 20

// Consistent is the loose sanity check of an excerpt against the line
// counts git logged at the turn's end (event.GitFacts, uncommitted changes
// against HEAD): the excerpt shows a part of those changes, so it may not
// add or remove more lines than they do. known is false when nothing was
// logged (the person interrupted the AI): there is nothing to check.
func Consistent(excerpt string, ins, del int, known bool) bool {
	if !known {
		return true
	}
	a, d := DiffLines(excerpt)
	return a <= ins+Slack && d <= del+Slack
}
