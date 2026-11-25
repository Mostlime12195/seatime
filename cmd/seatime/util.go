package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/emit"
	"seatime/internal/client/store"
)

func openStore() (*store.Store, error) { return store.Open(config.DBPath()) }

func openPipeline() (*store.Store, *emit.Pipeline, error) {
	st, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	p := emit.New(st, config.Home())
	p.NoText = !config.Load().KeepsPromptText()
	return st, p, nil
}

// style helpers: ANSI colour only on a capable terminal.
type styler struct{ on bool }

func newStyler() styler {
	if os.Getenv("NO_COLOR") != "" {
		return styler{}
	}
	fi, err := os.Stdout.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return styler{}
	}
	if runtime.GOOS == "windows" && os.Getenv("WT_SESSION") == "" && os.Getenv("ANSICON") == "" && os.Getenv("TERM") == "" {
		return styler{}
	}
	return styler{on: true}
}

func (s styler) c(code, t string) string {
	if !s.on {
		return t
	}
	return "\x1b[" + code + "m" + t + "\x1b[0m"
}
func (s styler) green(t string) string  { return s.c("32", t) }
func (s styler) orange(t string) string { return s.c("33", t) }
func (s styler) dim(t string) string    { return s.c("2", t) }
func (s styler) bold(t string) string   { return s.c("1", t) }
func (s styler) red(t string) string    { return s.c("31", t) }

func (s styler) bar(frac float64, width int, fill, empty string, color func(string) string) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	n := int(frac*float64(width) + 0.5)
	return color(strings.Repeat(fill, n)) + s.dim(strings.Repeat(empty, width-n))
}

func barGlyphs() (string, string) {
	if runtime.GOOS == "windows" && os.Getenv("WT_SESSION") == "" {
		return "#", "."
	}
	return "█", "░"
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// parseTime accepts RFC3339, YYYY-MM-DD, or a relative duration like 7d / 36h.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err == nil {
			return time.Now().AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("can't parse time %q (use 2006-01-02, RFC3339, 7d or 36h)", s)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
