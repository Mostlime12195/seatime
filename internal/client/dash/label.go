package dash

import (
	"encoding/csv"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"seatime/internal/client/stats"
	"seatime/internal/client/store"
)

// The labelling day view and the comparison export:
// ground truth for the one-page rule, kept on this computer.

type labelCell struct {
	Name, Label, Hint, Title string
}

type labelRow struct {
	Hour  string
	Cells []labelCell
}

type labelPage struct {
	Date, Prev, Next string
	Keys             []string
	Minutes          []string
	Rows             []labelRow
	Unlabelled       int
	Saved            string
}

func (s *Server) labelTmpl() *template.Template {
	return template.Must(template.ParseFS(assets, "label.html"))
}

func (s *Server) dayParam(r *http.Request) (time.Time, bool) {
	loc := s.Loc
	if v := r.URL.Query().Get("day"); v != "" {
		d, err := time.ParseInLocation("2006-01-02", v, loc)
		if err != nil {
			return time.Time{}, false
		}
		return d, true
	}
	return stats.StartOfDay(s.Now(), loc), true
}

func (s *Server) labelPage(day time.Time) (labelPage, error) {
	p := labelPage{Date: day.Format("2006-01-02"), Prev: day.AddDate(0, 0, -1).Format("2006-01-02"), Keys: store.LabelKeys}
	if next := day.AddDate(0, 0, 1); !next.After(s.Now()) {
		p.Next = next.Format("2006-01-02")
	}
	for m := 0; m < 60; m += 5 {
		p.Minutes = append(p.Minutes, fmt.Sprintf("%02d", m))
	}
	blocks, err := stats.DayBlocks(s.Store, day, s.Loc)
	if err != nil {
		return p, err
	}
	rows := map[string]*labelRow{}
	var order []string
	for _, b := range blocks {
		h := b.Start.Format("15:00")
		r := rows[h]
		if r == nil {
			r = &labelRow{Hour: h, Cells: make([]labelCell, 12)}
			rows[h] = r
			order = append(order, h)
		}
		var hint []string
		if b.Credited >= 1 {
			hint = append(hint, fmt.Sprintf("%.1fm", b.Credited/60))
		}
		for _, x := range []struct {
			n    int
			k, w string
		}{{b.Prompts, "P", "prompt"}, {b.Edits, "E", "edit"}, {b.Runs, "R", "run"}, {b.Agent, "A", "agent event"}, {b.Commits, "C", "commit"}} {
			if x.n > 0 {
				hint = append(hint, fmt.Sprintf("%s%d", x.k, x.n))
			}
		}
		title := fmt.Sprintf("%s-%s", b.Start.Format("15:04"), b.Start.Add(store.LabelBlock).Format("15:04"))
		if b.Credited >= 1 {
			title += fmt.Sprintf(", SeaTime counted %.1f min", b.Credited/60)
		}
		if len(hint) > 0 && b.Label == "" && (b.Prompts+b.Edits+b.Runs+b.Agent+b.Commits) > 0 {
			p.Unlabelled++
		}
		r.Cells[b.Start.Minute()/5] = labelCell{Name: "b" + strconv.FormatInt(store.BlockOf(b.Start), 10), Label: b.Label, Hint: strings.Join(hint, " "), Title: title}
	}
	for _, h := range order {
		p.Rows = append(p.Rows, *rows[h])
	}
	return p, nil
}

// sameOrigin refuses a form posted by another web page: the dashboard
// listens on loopback, which any page the user visits can address.
func sameOrigin(r *http.Request) bool {
	if v := r.Header.Get("Sec-Fetch-Site"); v != "" && v != "same-origin" && v != "none" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	return true
}

func (s *Server) handleLabel(w http.ResponseWriter, r *http.Request) {
	day, ok := s.dayParam(r)
	if !ok {
		http.Error(w, "day must look like 2026-11-20", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodPost {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		start, end := day, day.AddDate(0, 0, 1)
		ls := map[int64]string{}
		// a range fill first; blocks set one by one win
		if l := r.PostFormValue("fill_label"); l != "" {
			from, err1 := time.ParseInLocation("2006-01-02 15:04", day.Format("2006-01-02")+" "+r.PostFormValue("fill_from"), s.Loc)
			to, err2 := time.ParseInLocation("2006-01-02 15:04", day.Format("2006-01-02")+" "+r.PostFormValue("fill_to"), s.Loc)
			if err1 == nil && err2 == nil {
				for t := from.Truncate(store.LabelBlock); t.Before(to) && t.Before(end); t = t.Add(store.LabelBlock) {
					ls[store.BlockOf(t)] = l
				}
			}
		}
		for k, v := range r.PostForm {
			b, err := strconv.ParseInt(strings.TrimPrefix(k, "b"), 10, 64)
			if !strings.HasPrefix(k, "b") || err != nil || len(v) == 0 {
				continue
			}
			if t := store.BlockStart(b); t.Before(start) || !t.Before(end) {
				continue
			}
			if v[0] != "" || ls[b] == "" {
				ls[b] = v[0]
			}
		}
		if err := s.Store.SetLabels(ls); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/label?day=%s&saved=%d", day.Format("2006-01-02"), len(ls)), http.StatusSeeOther)
		return
	}
	p, err := s.labelPage(day)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	p.Saved = r.URL.Query().Get("saved")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; font-src 'self'; img-src data:; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	_ = s.labelTmpl().Execute(w, p)
}

// handleExport writes the comparison for the last `days` days (default 14),
// one row a day, in minutes.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	n := 14
	if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 && v <= 90 {
		n = v
	}
	today := stats.StartOfDay(s.Now(), s.Loc)
	rows, err := stats.Compare(s.Store, today.AddDate(0, 0, -(n-1)), today, s.Loc)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="seatime-truth.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	_ = WriteCompareCSV(w, rows)
}

// WriteCompareCSV writes the comparison rows as CSV, minutes to three
// decimals.
func WriteCompareCSV(w interface{ Write([]byte) (int, error) }, rows []stats.DayCompare) error {
	cw := csv.NewWriter(w)
	cw.Write([]string{"day", "seatime_rule_min", "hackatime_min", "third_rule_min", "labelled_min",
		"hands_on_min", "reading_min", "watching_min", "away_min", "unlabelled_active_blocks"})
	m := func(sec float64) string { return strconv.FormatFloat(sec/60, 'f', 3, 64) }
	for _, d := range rows {
		cw.Write([]string{d.Day.Format("2006-01-02"), m(d.Rule), m(d.Hackatime), m(d.ThirdRule), m(d.Labelled),
			m(d.ByLabel[store.LabelHandsOn]), m(d.ByLabel[store.LabelReading]), m(d.ByLabel[store.LabelWatching]), m(d.ByLabel[store.LabelAway]),
			strconv.Itoa(d.UnlabelledActive)})
	}
	cw.Flush()
	return cw.Error()
}
