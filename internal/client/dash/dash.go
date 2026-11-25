// Package dash is the personal dashboard served by `seatime dash` on
// 127.0.0.1. It reads only the local log (the one-page rule, pkg/ledger), so
// it works with no account and no network, and it loads nothing from
// anywhere else.
package dash

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"

	"seatime/internal/client/stats"
	"seatime/internal/client/store"
	"seatime/pkg/ledger"
)

//go:embed index.html label.html style.css fonts/*.woff2 fonts/OFL.txt
var assets embed.FS

type Server struct {
	Store *store.Store
	Port  int
	// GoalHours is the weekly goal (0 = stats.DefaultGoalHours).
	GoalHours float64
	// Counting answers "is SeaTime counting now?" (nil = not shown).
	Counting func() stats.Counting
	// Now and Loc are overridable for tests.
	Now  func() time.Time
	Loc  *time.Location
	tmpl *template.Template
}

type bar struct {
	Name, Value string
	Pct         float64
}

type dayView struct {
	Label, Value string
	Pct          float64
	Today        bool
}

type pageData struct {
	Now                  string
	Counting             *stats.Counting
	Today, Week, Longest string
	Goal                 string
	GoalPct              float64
	GoalLeft             string
	Streak               int
	HandsOn, HandsOnPast string
	HasPast, HasWeek     bool
	Projects, WeekProj   []bar
	Tools                []bar
	Days                 []dayView
	AINotCounted         string
	Empty, NothingToday  bool
	Rule                 template.HTML
}

func New(st *store.Store, port int) *Server {
	t := template.Must(template.ParseFS(assets, "index.html"))
	return &Server{Store: st, Port: port, tmpl: t, Now: time.Now, Loc: time.Local}
}

func (s *Server) data() (pageData, error) {
	now := s.Now()
	loc := s.Loc
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)
	sm, err := stats.Summarize(s.Store, now, loc, s.GoalHours)
	if err != nil {
		return pageData{}, err
	}
	d := pageData{Now: now.Format("Mon Jan 2, 15:04"), Today: dur(sm.Today), Week: dur(sm.Week), Streak: sm.Streak,
		Longest: dur(sm.Longest.Seconds()), HasPast: sm.HasPast, HasWeek: sm.Week > 0}
	if s.Counting != nil {
		c := s.Counting()
		d.Counting = &c
	}
	goal := sm.GoalHours * 3600
	d.Goal = dur(goal)
	d.GoalPct = min(sm.Week/goal*100, 100)
	if left := goal - sm.Week; left > 0 {
		d.GoalLeft = dur(left) + " to go"
	} else {
		d.GoalLeft = "goal reached"
	}
	d.HandsOn, d.HandsOnPast = pct(sm.HandsOn), pct(sm.HandsOnPast)
	d.Projects = bars(sm.TodayByProj)
	d.WeekProj = bars(sm.WeekByProj)
	d.Tools = bars(sm.WeekByTool)
	maxDay := 1.0
	for _, x := range sm.Days {
		maxDay = max(maxDay, x.Seconds)
	}
	for i, x := range sm.Days {
		d.Days = append(d.Days, dayView{Label: x.Day.Format("Mon"), Value: dur(x.Seconds), Pct: x.Seconds / maxDay * 100, Today: i == len(sm.Days)-1})
	}
	if sm.AINotCounted > 0 {
		d.AINotCounted = dur(sm.AINotCounted)
	}
	d.Empty = !sm.EverRecorded
	d.NothingToday = sm.Today == 0
	d.Rule = template.HTML(ruleHTML())
	return d, nil
}

func bars(ns []stats.Named) []bar {
	var out []bar
	top := 1.0
	for _, n := range ns {
		top = max(top, n.Seconds)
	}
	for _, n := range ns {
		out = append(out, bar{Name: n.Name, Value: dur(n.Seconds), Pct: n.Seconds / top * 100})
	}
	return out
}

func pct(f float64) string { return fmt.Sprintf("%d%%", int(f*100+0.5)) }

func dur(sec float64) string {
	m := int(sec/60 + 0.5)
	switch {
	case m == 0 && sec > 0:
		return "<1m"
	case m < 60:
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %02dm", m/60, m%60)
}

// ruleHTML is the rule in one paragraph, as docs/RULE.md states it.
func ruleHTML() string { return "<p>" + inline(ledger.Rule) + "</p>" }

func inline(s string) string {
	s = template.HTMLEscapeString(s)
	for _, m := range []struct{ md, open, close string }{{"**", "<b>", "</b>"}, {"`", "<code>", "</code>"}} {
		for {
			i := strings.Index(s, m.md)
			if i < 0 {
				break
			}
			j := strings.Index(s[i+len(m.md):], m.md)
			if j < 0 {
				break
			}
			j += i + len(m.md)
			s = s[:i] + m.open + s[i+len(m.md):j] + m.close + s[j+len(m.md):]
		}
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		d, err := s.data()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; font-src 'self'; img-src 'self' data:; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-store")
		_ = s.tmpl.Execute(w, d)
	})
	mux.HandleFunc("/api/today", func(w http.ResponseWriter, r *http.Request) {
		d, err := s.data()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(d)
	})
	mux.HandleFunc("/label", s.handleLabel)
	mux.HandleFunc("/export.csv", s.handleExport)
	mux.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		b, _ := assets.ReadFile("style.css")
		w.Header().Set("Content-Type", "text/css")
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = w.Write(b)
	})
	// the page's two typefaces, embedded: nothing is fetched from elsewhere
	mux.HandleFunc("/fonts/", func(w http.ResponseWriter, r *http.Request) {
		b, err := assets.ReadFile(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil || !strings.HasSuffix(r.URL.Path, ".woff2") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("Cache-Control", "max-age=86400")
		_, _ = w.Write(b)
	})
	// Reject DNS rebinding: only answer for our loopback host names.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, _, _ := net.SplitHostPort(r.Host)
		if h == "" {
			h = r.Host
		}
		if !(h == "127.0.0.1" || strings.EqualFold(h, "localhost")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Serve listens on 127.0.0.1:port (blocking).
func (s *Server) Serve(ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}
