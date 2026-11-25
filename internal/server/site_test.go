package server

import (
	"regexp"
	"strings"
	"testing"

	"seatime/pkg/chain"
)

var hrefRe = regexp.MustCompile(`(?:href|src|action)="([^"]+)"`)

// Crawl the site as a visitor and as a logged-in user: every same-site link
// answers (no 404, no 5xx), and no page loads anything from another origin
// (no CDNs, fonts, trackers or third-party images).
func TestSiteLinksAndNoThirdPartyLoads(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("kid")
	const project = "local:parser"
	evs, heads := localChain(t, project)
	d.sync(chain.Checkpoint{Seq: int64(len(evs)), Hash: heads[len(heads)-1]}, nil, nil)
	for _, b := range []*browser{e.browser(), br} {
		seen := map[string]bool{}
		queue := []string{"/", "/leaderboard", "/@kid"}
		for len(queue) > 0 && len(seen) < 80 {
			p := queue[0]
			queue = queue[1:]
			if seen[p] {
				continue
			}
			seen[p] = true
			code, body := b.get(p)
			if code == 404 || code >= 500 {
				t.Errorf("%s answers %d", p, code)
				continue
			}
			for _, m := range hrefRe.FindAllStringSubmatch(body, -1) {
				u := m[1]
				tag := m[0]
				if strings.HasPrefix(tag, "src=") && (strings.HasPrefix(u, "http") || strings.HasPrefix(u, "//")) {
					t.Errorf("%s loads %s from another origin", p, u)
				}
				if strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//") && !strings.Contains(u, "logout") &&
					!strings.HasPrefix(tag, "action=") && !strings.HasPrefix(u, "/auth/") && !strings.HasPrefix(u, "/dl/") {
					if i := strings.IndexAny(u, "?#"); i >= 0 {
						u = u[:i]
					}
					queue = append(queue, u)
				}
			}
			if strings.Contains(body, "<link") && regexp.MustCompile(`<link[^>]+href="(https?:)?//`).MatchString(body) {
				t.Errorf("%s links a stylesheet from another origin", p)
			}
		}
		if len(seen) < 5 {
			t.Fatalf("crawl found only %v", seen)
		}
	}
}
