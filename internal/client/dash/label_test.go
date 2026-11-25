package dash

import (
	"io"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/store"
)

func post(t *testing.T, s *Server, path string, form url.Values, origin string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "http://127.0.0.1"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b)
}

// Labelling a day: the view shows the day's active span in 5-minute blocks,
// one select each (Tab, then h/r/w/a: two keys a block, no script), a range
// fill, and the export reflects what was saved. A page on another origin
// cannot post labels.
func TestLabelADay(t *testing.T) {
	s := newServer(t, true)
	day := "2026-11-20" // seedWeek: edits 15:00-15:20, prompts 15:25-15:50
	code, page := get(t, s, "/label?day="+day)
	if code != 200 || !strings.Contains(page, "What were you doing? "+day) {
		t.Fatalf("%d %s", code, page)
	}
	selects := regexp.MustCompile(`<select name="b(\d+)"`).FindAllStringSubmatch(page, -1)
	if len(selects) < 12 || len(selects) > 72 {
		t.Fatalf("%d blocks shown", len(selects))
	}
	if !strings.Contains(page, "<option value=\"hands-on\">") || strings.Contains(page, "<script") {
		t.Fatal("the four keys, and no script")
	}
	t.Logf("%d blocks to label: %d key presses with Tab and a letter", len(selects), 2*len(selects))

	d, _ := time.Parse("2006-01-02", day)
	at := func(h, m int) string {
		return "b" + itoa(store.BlockOf(d.Add(time.Duration(h)*time.Hour+time.Duration(m)*time.Minute)))
	}
	form := url.Values{"fill_from": {"15:00"}, "fill_to": {"15:20"}, "fill_label": {store.LabelHandsOn},
		at(15, 25): {store.LabelReading}, at(15, 30): {store.LabelWatching}, at(15, 35): {store.LabelAway}, at(15, 5): {store.LabelReading}}
	if code, _ := post(t, s, "/label?day="+day, form, "https://evil.example"); code != 403 {
		t.Fatalf("a cross-site post: %d", code)
	}
	if code, _ := post(t, s, "/label?day="+day, form, "http://127.0.0.1"); code != 303 {
		t.Fatalf("save: %d", code)
	}
	code, page = get(t, s, "/label?day="+day+"&saved=7")
	if code != 200 || !strings.Contains(page, "Saved 7 blocks") || !strings.Contains(page, `<option value="watching" selected>`) {
		t.Fatalf("after saving: %d", code)
	}
	code, csv := get(t, s, "/export.csv?days=2")
	if code != 200 {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimSpace(csv), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "day,seatime_rule_min,hackatime_min,third_rule_min,labelled_min") {
		t.Fatalf("%q", csv)
	}
	// 15:00-15:20 hands-on except 15:05 (reading, set one by one), 15:25
	// reading, 15:30 watching, 15:35 away: 15 + 10 + 5 minutes at work
	f := strings.Split(lines[2], ",")
	if f[0] != day || f[4] != "30.000" || f[5] != "15.000" || f[6] != "10.000" || f[7] != "5.000" || f[8] != "5.000" {
		t.Fatalf("today's row: %s", lines[2])
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
