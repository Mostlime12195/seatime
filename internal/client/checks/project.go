package checks

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"seatime/internal/client/scrub"
	"seatime/pkg/promptcheck"
)

// Bounds on reading a project's folder for its facts. The hook does it
// while the person waits for the AI tool, so it is short.
const (
	factsTime  = 300 * time.Millisecond
	walkFiles  = 2000 // entries a folder without git is walked for
	walkDepth  = 4
	listBytes  = 256 << 10 // of git's file list
	readmeRead = 16 << 10
)

var readmeRe = regexp.MustCompile(`(?i)^readme(\.(md|markdown|txt|rst))?$`)

// Facts reads what a check says about the project (promptcheck.Project) in
// its folder root: name as given, the file names (git's list of tracked
// and untracked files without the ignored ones when root is a git
// checkout, else a short walk), cut to the shallowest that fit, and the
// first lines of the README in root with secrets blanked. Only names and
// the README are read, never another file's contents. The home folder and
// a file system's root are no project: for them only the name is given.
func Facts(ctx context.Context, root, name, home string) *promptcheck.Project {
	name, _ = promptcheck.Cut(name, promptcheck.MaxProjectName)
	p := &promptcheck.Project{Name: name}
	if root == "" || !filepath.IsAbs(root) {
		return p
	}
	root = filepath.Clean(root)
	if root == filepath.Dir(root) || home != "" && root == filepath.Clean(home) {
		return p
	}
	ctx, cancel := context.WithTimeout(ctx, factsTime)
	defer cancel()
	paths, ok := gitFiles(ctx, root)
	if !ok {
		paths = walkFolder(ctx, root)
	}
	p.Files, p.Total = promptcheck.Tree(paths, promptcheck.MaxTree)
	for _, f := range paths {
		if !strings.Contains(f, "/") && readmeRe.MatchString(f) {
			p.Readme = readme(filepath.Join(root, f))
			break
		}
	}
	return p
}

// gitFiles lists root's files as git sees them: tracked, and untracked
// but not ignored. ok is false when root is not a git checkout or git is
// missing. A list cut short by the time or size bound is used as far as
// it came.
func gitFiles(ctx context.Context, root string) (paths []string, ok bool) {
	if _, err := os.Lstat(filepath.Join(root, ".git")); err != nil {
		return nil, false
	}
	var b bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Stdout = &limitWriter{&b, listBytes}
	if err := cmd.Run(); err != nil && (b.Len() == 0 || !errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		return nil, false
	}
	out := b.Bytes()
	if i := bytes.LastIndexByte(out, 0); i >= 0 {
		out = out[:i] // a last name may be cut short
	} else {
		out = nil
	}
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			paths = append(paths, string(f))
		}
	}
	return paths, true
}

// walkFolder lists the files of a folder that is not a git checkout,
// level by level: at most walkFiles entries, walkDepth levels deep, no
// hidden, vendored or build folders, no links followed.
func walkFolder(ctx context.Context, root string) []string {
	var paths []string
	level := []string{""}
	seen := 0
	for depth := 0; depth < walkDepth && len(level) > 0; depth++ {
		var next []string
		for _, dir := range level {
			if ctx.Err() != nil || seen >= walkFiles {
				return paths
			}
			es, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
			if err != nil {
				continue
			}
			for _, e := range es {
				if seen++; seen > walkFiles {
					return paths
				}
				rel := e.Name()
				if dir != "" {
					rel = dir + "/" + rel
				}
				switch {
				case e.IsDir():
					if !strings.HasPrefix(e.Name(), ".") && !promptcheck.SkipDir(e.Name()) {
						next = append(next, rel)
					}
				case e.Type().IsRegular():
					paths = append(paths, rel)
				}
			}
		}
		level = next
	}
	return paths
}

// readme is the top of a README: a regular file only (a link could point
// anywhere), text only, secrets blanked as in prompts.
func readme(path string) string {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b := make([]byte, readmeRead)
	n, _ := f.Read(b)
	b = b[:n]
	if bytes.IndexByte(b, 0) >= 0 {
		return ""
	}
	s, _ := promptcheck.Cut(strings.TrimSpace(scrub.Text(strings.ToValidUTF8(string(b), ""))), promptcheck.MaxReadme)
	return s
}
