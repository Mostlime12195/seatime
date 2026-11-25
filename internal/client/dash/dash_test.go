package dash

import (
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/stats"
	"seatime/internal/client/store"
	"seatime/pkg/event"
)

var now = time.Date(2026, 11, 20, 17, 30, 0, 0, time.UTC)

func seedWeek(t *testing.T, st *store.Store) {
	t.Helper()
	add := func(e event.Event) {
		if _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	for day := 34; day >= 0; day-- {
		if day%9 == 5 {
			continue // a day off now and then
		}
		base := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC).AddDate(0, 0, -day)
		proj := []string{"github.com/kid/game", "local:site", "github.com/kid/bot"}[day%3]
		add(event.Event{Kind: event.Idle, Harness: "seatime", TS: base.Add(2 * time.Hour), WindowMS: (2 * time.Hour).Milliseconds(), IdleSrc: "ioreg"})
		for m := 0; m < 40; m++ {
			add(event.Event{Kind: event.Edit, Harness: "editor:vscode", Project: proj, TS: base.Add(time.Duration(m) * 30 * time.Second)})
		}
		sid := fmt.Sprintf("s%d", day)
		for i := 0; i < 6; i++ {
			p := base.Add(25*time.Minute + time.Duration(i)*5*time.Minute)
			text := []string{"add a score counter", "the counter resets on pause, keep it", "fix it", "still broken", "try again", "it still does not work"}[i]
			add(event.Event{Kind: event.Prompt, Harness: "claude-code", SessionID: sid, Project: proj, TS: p, Text: text, Words: len(strings.Fields(text)), Chars: len([]rune(text))}) // the pipeline records lengths (emit)
			add(event.Event{Kind: event.AgentStart, Harness: "claude-code", SessionID: sid, Project: proj, TS: p})
			add(event.Event{Kind: event.AgentStop, Harness: "claude-code", SessionID: sid, Project: proj, TS: p.Add(3 * time.Minute)})
		}
	}
}

func newServer(t *testing.T, seed bool) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if seed {
		seedWeek(t, st)
	}
	s := New(st, 0)
	s.Now, s.Loc = func() time.Time { return now }, time.UTC
	s.Counting = func() stats.Counting { return stats.Counting{Yes: true, Project: "game", Reason: "hand editing"} }
	return s
}

func get(t *testing.T, s *Server, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "http://127.0.0.1"+path, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b)
}

func TestSeededWeekRendersEveryPanel(t *testing.T) {
	s := newServer(t, true)
	start := time.Now()
	code, page := get(t, s, "/")
	elapsed := time.Since(start)
	if code != 200 {
		t.Fatalf("%d %s", code, page)
	}
	for _, want := range []string{"counting now: yes, on game", "today", "last 7 days", "day streak", "longest focused stretch",
		"Weekly goal", "Projects today", "Projects this week", "By tool this week", "claude-code", "vscode (by hand)",
		"Hands-on share", "four weeks before", "Agent time not counted today", "counted up to 2 minutes", "How SeaTime counts"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, banned := range []string{"tier", "Tier", "verified", "spot check", "reveal", "ruleset", "struggle", "dark flow"} {
		if strings.Contains(page, banned) {
			t.Errorf("old vocabulary %q on the page", banned)
		}
	}
	// nothing loaded from anywhere else: no absolute URLs at all
	if m := regexp.MustCompile(`(?i)(src|href)="(https?:)?//`).FindString(page); m != "" {
		t.Errorf("page loads something from outside: %s", m)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("first render took %s (budget 200 ms)", elapsed)
	}
	t.Logf("rendered %d bytes in %s", len(page), elapsed.Round(time.Millisecond))
	if code, css := get(t, s, "/style.css"); code != 200 || !strings.Contains(css, "max-width:640px") {
		t.Fatal("stylesheet with a phone layout")
	}
}

func TestEmptyStateForNewUsers(t *testing.T) {
	s := newServer(t, false)
	s.Counting = func() stats.Counting {
		return stats.Counting{Reason: "nothing has happened in your AI tools or editor in the last 30 min"}
	}
	code, page := get(t, s, "/")
	if code != 200 || !strings.Contains(page, "Nothing recorded yet") || !strings.Contains(page, "seatime doctor") || !strings.Contains(page, "counting now: no") {
		t.Fatalf("%d %s", code, page)
	}
}

func TestRejectsOtherHosts(t *testing.T) {
	s := newServer(t, false)
	req := httptest.NewRequest("GET", "http://evil.example/", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("DNS-rebinding host answered: %d", w.Code)
	}
	if code, _ := get(t, s, "/nope"); code != 404 {
		t.Fatalf("unknown path: %d", code)
	}
}

// With SEATIME_DASH_SHOT_DIR set, write the seeded page and stylesheet there
// (for screenshots with a headless browser).
func TestWriteSeededPageForScreenshots(t *testing.T) {
	dir := os.Getenv("SEATIME_DASH_SHOT_DIR")
	if dir == "" {
		t.Skip("set SEATIME_DASH_SHOT_DIR")
	}
	s := newServer(t, true)
	_, page := get(t, s, "/")
	_, css := get(t, s, "/style.css")
	os.WriteFile(filepath.Join(dir, "index.html"), []byte(strings.Replace(page, `href="/style.css"`, `href="style.css"`, 1)), 0o644)
	os.WriteFile(filepath.Join(dir, "style.css"), []byte(css), 0o644)
}
