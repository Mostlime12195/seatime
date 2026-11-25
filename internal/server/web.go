package server

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"seatime/internal/server/store"
)

//go:embed web/templates/*.html web/static/*
var webFS embed.FS

type pages struct {
	set map[string]*template.Template
}

// The public source repository, and Hack Club's privacy notice and terms,
// linked from every page's footer.
const (
	sourceURL  = "https://github.com/Mostlime12195/seatime"
	privacyURL = "https://hackclub.com/privacy-and-terms#hack-club-privacy-notice"
	termsURL   = "https://hackclub.com/privacy-and-terms#hack-club-standard-terms-and-conditions"
)

// baseData is what every template receives; page-specific data is in .Data.
type baseData struct {
	Title   string
	User    *store.User
	CSRF    string
	Dev     bool
	BaseURL string
	Flash   string
	Data    any
	// footer links
	Source, Privacy, Terms string
}

func (s *Server) base(ss *session, u *store.User, title string) *baseData {
	b := &baseData{Title: title, User: u, Dev: s.cfg.Dev, BaseURL: s.cfg.BaseURL,
		Source: sourceURL, Privacy: privacyURL, Terms: termsURL}
	if ss != nil {
		b.CSRF = ss.CSRF
	}
	return b
}

func fmtDur(sec float64) string {
	if sec <= 0 {
		return "0m"
	}
	m := int(sec/60 + 0.5)
	if m == 0 {
		return "<1m"
	}
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %02dm", m/60, m%60)
}

// fmtDurHTML is fmtDur with the unit letters set small beside the numerals.
func fmtDurHTML(sec float64) template.HTML {
	var b strings.Builder
	for _, c := range fmtDur(sec) {
		switch c {
		case 'h', 'm':
			b.WriteString(`<i class="u">` + string(c) + `</i>`)
		case ' ':
		case '<':
			b.WriteString("&lt;")
		default:
			b.WriteRune(c)
		}
	}
	return template.HTML(b.String())
}

func fmtHours(sec float64) string {
	h := sec / 3600
	if h >= 100 {
		return fmt.Sprintf("%.0f", h)
	}
	return fmt.Sprintf("%.1f", h)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < 90*time.Second:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func loadPages(s *Server) (*pages, error) {
	funcs := template.FuncMap{
		"dur":  fmtDur,
		"unck": uncheckedLabel,
		"durx": fmtDurHTML,
		// a flag reads "code: detail"; the page sets the two apart
		"flagk": func(f string) string { k, _, _ := strings.Cut(f, ": "); return k },
		"flagv": func(f string) string { _, v, _ := strings.Cut(f, ": "); return v },
		"hours": fmtHours,
		"ago":   ago,
		"osname": func(goos string) string {
			if n, ok := map[string]string{"darwin": "macOS", "linux": "Linux", "windows": "Windows", "freebsd": "FreeBSD"}[goos]; ok {
				return n
			}
			return goos
		},
		"date": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		"pct": func(a, b float64) string {
			if b <= 0 {
				return "0"
			}
			return fmt.Sprintf("%.0f", 100*a/b)
		},
		"short": func(s string) string {
			if len(s) > 8 {
				return s[:8]
			}
			return s
		},
		"add":  func(a, b int) int { return a + b },
		"safe": func(s string) template.HTML { return template.HTML(s) },
		"tzh":  func(min int) int { return min / 60 },
	}
	p := &pages{set: map[string]*template.Template{}}
	entries, err := fs.ReadDir(webFS, "web/templates")
	if err != nil {
		return nil, err
	}
	// partials (_name.html) are parsed into every page
	common := []string{"web/templates/layout.html"}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "_") && strings.HasSuffix(e.Name(), ".html") {
			common = append(common, "web/templates/"+e.Name())
		}
	}
	for _, e := range entries {
		n := e.Name()
		if n == "layout.html" || strings.HasPrefix(n, "_") || !strings.HasSuffix(n, ".html") {
			continue
		}
		t, err := template.New(n).Funcs(funcs).ParseFS(webFS, append(common, "web/templates/"+n)...)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", n, err)
		}
		p.set[strings.TrimSuffix(n, ".html")] = t
	}
	return p, nil
}

func (p *pages) exec(w http.ResponseWriter, status int, page, tmpl string, b *baseData, data any) {
	t := p.set[page]
	if t == nil {
		http.Error(w, "no such page", 500)
		return
	}
	b.Data = data
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, tmpl, b); err != nil {
		http.Error(w, "template error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (p *pages) render(w http.ResponseWriter, r *http.Request, page string, b *baseData, data any) {
	p.exec(w, 200, page, "layout", b, data)
}

// fragment renders just the named live-updating block.
func (p *pages) fragment(w http.ResponseWriter, page, tmpl string, b *baseData, data any) {
	p.exec(w, 200, page, tmpl, b, data)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	ss, u := s.session(w, r)
	s.pages.exec(w, 404, "error", "layout", s.base(ss, u, "Not found"), map[string]any{"Code": 404, "Msg": "We couldn't find that page."})
}

func (s *Server) staticHandler() http.Handler {
	sub, _ := fs.Sub(webFS, "web/static")
	h := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}
