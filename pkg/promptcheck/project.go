package promptcheck

import (
	"fmt"
	"sort"
	"strings"
)

// Caps on the project facts of a check request, in bytes.
const (
	MaxProjectName = 200
	MaxTree        = 2 << 10
	MaxReadme      = 2 << 10
)

// Project is what a check says about the project itself: its name, some
// of its file names and the top of its README, as the device read them in
// the project's folder when the judged prompt was sent (before the AI
// changed anything for it). Like the prompt, it is used for the call and
// never kept. A device older than this field sends none.
type Project struct {
	Name string `json:"name,omitempty"`
	// Files are paths inside the project, one per line (Tree); Total is how
	// many files the device found, of which Files may show only some.
	Files  string `json:"files,omitempty"`
	Total  int    `json:"total,omitempty"`
	Readme string `json:"readme,omitempty"`
}

// skipDirs are folders whose files say nothing about the project:
// vendored code, caches and build output.
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true, "third_party": true, "bower_components": true, "Pods": true,
	".venv": true, "venv": true, "__pycache__": true, ".tox": true, ".mypy_cache": true, ".pytest_cache": true,
	".next": true, ".nuxt": true, ".cache": true, ".gradle": true, ".idea": true, "DerivedData": true,
	"dist": true, "build": true, "target": true, "coverage": true,
}

// SkipDir reports whether a folder of that name is left out of the tree.
func SkipDir(name string) bool { return skipDirs[name] }

// Tree picks the file names a check shows from a project's paths (slashes,
// relative to its root): files under a vendored or build folder are left
// out, then the shallowest paths are taken until max bytes are full, and
// shown in path order. total counts the files that were not left out.
func Tree(paths []string, max int) (files string, total int) {
	type entry struct {
		path  string
		depth int
	}
	var es []entry
	seen := map[string]bool{}
next:
	for _, p := range paths {
		p = strings.Trim(p, "/")
		if p == "" || len(p) > 200 || seen[p] || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			continue
		}
		segs := strings.Split(p, "/")
		for _, s := range segs[:len(segs)-1] {
			if skipDirs[s] {
				continue next
			}
		}
		seen[p] = true
		es = append(es, entry{p, len(segs)})
	}
	total = len(es)
	sort.Slice(es, func(i, j int) bool {
		if es[i].depth != es[j].depth {
			return es[i].depth < es[j].depth
		}
		return es[i].path < es[j].path
	})
	var take []string
	n := 0
	for _, e := range es {
		if n+len(e.path)+1 > max {
			break
		}
		n += len(e.path) + 1
		take = append(take, e.path)
	}
	sort.Strings(take)
	return strings.Join(take, "\n"), total
}

// block is the project's section of the state. Every line a person could
// have written (the name, a file name, the README) goes through Neutral.
// A check without project facts (an older device) says so, so the question
// about the project always has a section to read.
func (p *Project) block() string {
	var b strings.Builder
	b.WriteString("=== PROJECT (the person's own folder name, file names and README, as they were when the judged prompt was sent) ===\n")
	if p == nil || p.Name == "" && p.Files == "" && p.Readme == "" {
		b.WriteString("(the tracker sent no facts about the project)\n")
	} else {
		name := strings.Join(strings.Fields(p.Name), " ")
		if name == "" {
			name = "(none)"
		}
		fmt.Fprintf(&b, "PROJECT NAME: %s\n", Neutral(name))
		if files := strings.TrimSpace(p.Files); files == "" {
			b.WriteString("PROJECT FILES: (none)\n")
		} else {
			shown := strings.Count(files, "\n") + 1
			if p.Total > shown {
				fmt.Fprintf(&b, "PROJECT FILES (%d of %d, the shallowest):\n", shown, p.Total)
			} else {
				fmt.Fprintf(&b, "PROJECT FILES (%d):\n", shown)
			}
			b.WriteString(Neutral(files) + "\n")
		}
		if readme := strings.TrimSpace(p.Readme); readme == "" {
			b.WriteString("PROJECT README: (none)\n")
		} else {
			b.WriteString("PROJECT README (its first lines):\n" + Neutral(readme) + "\n")
		}
	}
	b.WriteString("=== END OF PROJECT ===\n\n")
	return b.String()
}
