package server

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/ledger"
)

// Counted time and prompts by AI model: a plain breakdown of the counted
// total, on the owner's own pages (from synced time) and in what an
// application reads for a project (from checked time). Hand time has no
// model and is its own row; AI time whose records name no model is "not
// recorded". Nothing here changes a number or judges a model.

// cleanModels keeps a synced bucket's model split inside the bucket: at
// most 8 names, each at most 120 printable characters, with no more seconds
// than the bucket's AI seconds and no more prompts than its prompts.
func cleanModels(b ledger.Bucket) []ledger.ModelUse {
	var out []ledger.ModelUse
	sec, prompts := 0.0, 0
	for _, m := range b.Models {
		name := strings.TrimSpace(strings.Map(func(r rune) rune {
			if !unicode.IsPrint(r) {
				return -1
			}
			return r
		}, m.Model))
		if len(name) > 120 {
			name = strings.ToValidUTF8(name[:120], "")
		}
		if name == "" || len(out) == 8 || !finite(m.Seconds) {
			continue
		}
		m.Model, m.Seconds, m.Prompts = name, max(m.Seconds, 0), min(max(m.Prompts, 0), b.Prompts-prompts)
		if m.Seconds == 0 && m.Prompts == 0 {
			continue
		}
		sec += m.Seconds
		prompts += m.Prompts
		out = append(out, m)
	}
	if sec > b.AISeconds && sec > 0 {
		for i := range out {
			out[i].Seconds *= b.AISeconds / sec
		}
	}
	return out
}

var modelDate = regexp.MustCompile(`[-@_](20\d{2}-?\d{2}-?\d{2}|latest)$`)

// modelName is a model's name as a page shows it: the name the AI tool
// reported, without a trailing release date ("claude-opus-5-20260115",
// "gpt-5.2@2026-01-15"). Display only: stored and served names are as
// reported.
func modelName(raw string) string {
	name := strings.TrimSpace(raw)
	if short := modelDate.ReplaceAllString(name, ""); short != "" {
		name = short
	}
	return name
}

// modelRow is one row of the breakdown on a page.
type modelRow struct {
	Name    string
	Title   string // the names as reported, when Name shortens them
	Class   string // the bar: "a" a model, "h" by hand, "n" not recorded
	Seconds float64
	Prompts int
	Pct     int
	Time    string // whole minutes; the rows' minutes add up to the total's
}

// modelRows turns sums into a page's rows for a counted total: models by
// counted time, then hand time, then time with no model recorded. Seconds
// of the total that no row holds (none, unless the rollup is behind) go to
// "not recorded", so the rows always add up to the total the page shows.
// No rows when there is nothing but hand time.
func modelRows(sums []store.ModelSum, total float64) []modelRow {
	byName := map[string]*modelRow{}
	raw := map[string][]string{}
	var rows []*modelRow
	hand := &modelRow{Name: "By hand", Class: "h"}
	none := &modelRow{Name: "Model not recorded", Class: "n"}
	sum := 0.0
	for _, m := range sums {
		m.Seconds = max(m.Seconds, 0)
		sum += m.Seconds
		r := none
		switch {
		case m.Hand:
			r = hand
		case m.Model != "":
			name := modelName(m.Model)
			if r = byName[name]; r == nil {
				r = &modelRow{Name: name, Class: "a"}
				byName[name] = r
				rows = append(rows, r)
			}
			if !slices.Contains(raw[name], m.Model) {
				raw[name] = append(raw[name], m.Model)
			}
		}
		r.Seconds += m.Seconds
		r.Prompts += max(m.Prompts, 0)
	}
	if d := total - sum; d > 0 {
		none.Seconds += d
	}
	for _, r := range rows {
		if names := raw[r.Name]; len(names) > 1 || names[0] != r.Name {
			slices.Sort(names)
			r.Title = strings.Join(names, ", ")
		}
	}
	rows = slices.DeleteFunc(rows, func(r *modelRow) bool { return r.Seconds < 0.5 && r.Prompts == 0 })
	slices.SortStableFunc(rows, func(a, b *modelRow) int {
		return cmp.Or(cmp.Compare(b.Seconds, a.Seconds), cmp.Compare(a.Name, b.Name))
	})
	if len(rows) == 0 && none.Seconds < 0.5 && none.Prompts == 0 {
		return nil
	}
	if hand.Seconds >= 0.5 {
		rows = append(rows, hand)
	}
	if none.Seconds >= 0.5 || none.Prompts > 0 {
		rows = append(rows, none)
	}
	// whole minutes that add up to the total as the page shows it
	want := int(total/60 + 0.5)
	mins := make([]int, len(rows))
	order := make([]int, len(rows))
	got := 0
	for i, r := range rows {
		mins[i], order[i] = int(r.Seconds/60), i
		got += mins[i]
	}
	frac := func(i int) float64 { return rows[i].Seconds/60 - math.Floor(rows[i].Seconds/60) }
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(frac(b), frac(a)) })
	for n := 0; got < want && n < len(order); n++ {
		mins[order[n]]++
		got++
	}
	out := make([]modelRow, len(rows))
	for i, r := range rows {
		r.Time = fmtDur(float64(mins[i]) * 60)
		if mins[i] == 0 && r.Seconds > 0 {
			r.Time = "<1m"
		}
		if total > 0 {
			r.Pct = int(100*r.Seconds/total + 0.5)
		}
		out[i] = *r
	}
	return out
}

// modelsJSON is the breakdown an application reads: one row per model as
// reported (sorted by name), then hand time, then what has no model on
// record. The rows' seconds add up to total, the project's checked seconds
// in the range: whatever the stored rows do not hold (time checked before
// models were kept) is in "not_recorded".
func modelsJSON(sums []store.ModelSum, total float64) []api.ModelTime {
	out := []api.ModelTime{}
	hand := api.ModelTime{Kind: api.ModelHand}
	none := api.ModelTime{Kind: api.ModelNotRecorded}
	held := 0.0
	for _, m := range sums {
		m.Seconds = max(m.Seconds, 0)
		switch {
		case m.Hand:
			hand.Seconds += m.Seconds
			held += m.Seconds
		case m.Model == "":
			none.Prompts += m.Prompts
		default:
			if m.Seconds > 0 || m.Prompts > 0 {
				out = append(out, api.ModelTime{Kind: api.ModelNamed, Model: m.Model, Seconds: round3(m.Seconds), Prompts: m.Prompts})
			}
			held += m.Seconds
		}
	}
	slices.SortStableFunc(out, func(a, b api.ModelTime) int { return cmp.Compare(a.Model, b.Model) })
	none.Seconds = max(total-held, 0)
	for _, r := range []api.ModelTime{hand, none} {
		if r.Seconds = round3(r.Seconds); r.Seconds > 0 || r.Prompts > 0 {
			out = append(out, r)
		}
	}
	return out
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
