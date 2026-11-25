package ledger

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"seatime/pkg/event"
)

// BucketWidth is the width of a synced bucket.
const BucketWidth = time.Minute

// Bucket is one (project, minute): the unit a device syncs to the server,
// and what the server adds up. The JSON names are the wire names.
type Bucket struct {
	Project string    `json:"project"`
	Start   time.Time `json:"start"` // UTC, on a minute boundary
	// CodingSeconds and AISeconds are counted time, split by the category of
	// the heartbeat that ends each gap. Their sum is the number shown as hours.
	CodingSeconds float64 `json:"coding_seconds"`
	AISeconds     float64 `json:"ai_seconds"`
	// AINotCountedSeconds is the agent's time outside the 10 minutes after
	// one of your heartbeats (a fact; never part of the hours).
	AINotCountedSeconds float64  `json:"ai_not_counted_seconds"`
	Harnesses           []string `json:"harnesses,omitempty"`
	Prompts             int      `json:"prompts"`
	Edits               int      `json:"edits"`
	Commits             int      `json:"commits"`
	Runs                int      `json:"runs"`
	// Models is the part of AISeconds and Prompts whose records name a
	// model, by model, sorted by name. What they leave of AISeconds and
	// Prompts has no model recorded; hand time (CodingSeconds) never has one.
	// A total (Sum) carries none.
	Models []ModelUse `json:"models,omitempty"`
}

// ModelUse is counted AI seconds and prompts with one model, named as the
// AI tool reported it.
type ModelUse struct {
	Model   string  `json:"model"`
	Seconds float64 `json:"seconds,omitempty"`
	Prompts int     `json:"prompts,omitempty"`
}

// addModel adds seconds and prompts with a model to the bucket.
func (b *Bucket) addModel(model string, sec float64, prompts int) {
	if model == "" {
		return
	}
	for i := range b.Models {
		if b.Models[i].Model == model {
			b.Models[i].Seconds += sec
			b.Models[i].Prompts += prompts
			return
		}
	}
	b.Models = append(b.Models, ModelUse{Model: model, Seconds: sec, Prompts: prompts})
}

// ScaleCounted multiplies the bucket's counted seconds, and each model's
// share of them, by f.
func (b *Bucket) ScaleCounted(f float64) {
	b.CodingSeconds *= f
	b.AISeconds *= f
	if len(b.Models) > 0 {
		b.Models = slices.Clone(b.Models)
		for i := range b.Models {
			b.Models[i].Seconds *= f
		}
	}
}

// Add adds o's time and counts to b: the same minute of the same project,
// which a device recorded under two of the project's keys.
func (b *Bucket) Add(o Bucket) {
	b.CodingSeconds += o.CodingSeconds
	b.AISeconds += o.AISeconds
	b.AINotCountedSeconds += o.AINotCountedSeconds
	b.Prompts += o.Prompts
	b.Edits += o.Edits
	b.Commits += o.Commits
	b.Runs += o.Runs
	b.Harnesses = slices.Clone(b.Harnesses)
	for _, h := range o.Harnesses {
		if !slices.Contains(b.Harnesses, h) {
			b.Harnesses = append(b.Harnesses, h)
		}
	}
	slices.Sort(b.Harnesses)
	b.Models = slices.Clone(b.Models)
	for _, m := range o.Models {
		b.addModel(m.Model, m.Seconds, m.Prompts)
	}
	slices.SortFunc(b.Models, func(x, y ModelUse) int { return cmp.Compare(x.Model, y.Model) })
}

// Seconds is the bucket's counted time: hand plus AI.
func (b Bucket) Seconds() float64 { return b.CodingSeconds + b.AISeconds }

// Buckets cuts a result into (project, minute) buckets: seconds from its
// segments, counts from evs (activity inside [r.From, r.To) only). Rows are
// sorted by start, then project; their seconds sum to the result's.
func Buckets(r Result, evs []event.Event) []Bucket {
	type key struct {
		min  int64
		proj string
	}
	m := map[key]*Bucket{}
	get := func(t time.Time, p string) *Bucket {
		k := key{t.Unix() / 60, p}
		b := m[k]
		if b == nil {
			b = &Bucket{Project: p, Start: time.Unix(k.min*60, 0).UTC()}
			m[k] = b
		}
		return b
	}
	addHarness := func(b *Bucket, h string) {
		if h != "" && !slices.Contains(b.Harnesses, h) {
			b.Harnesses = append(b.Harnesses, h)
		}
	}
	for _, s := range r.Segments {
		for a := s.Start; a.Before(s.End); {
			b := a.Truncate(BucketWidth).Add(BucketWidth)
			if s.End.Before(b) {
				b = s.End
			}
			sec, bk := b.Sub(a).Seconds(), get(a, s.Project)
			switch {
			case !s.Counted:
				bk.AINotCountedSeconds += sec
			case s.Category == Coding:
				bk.CodingSeconds += sec
			default:
				bk.AISeconds += sec
				bk.addModel(s.Model, sec, 0)
			}
			if s.Counted {
				addHarness(bk, s.Tool)
			}
			a = b
		}
	}
	for _, e := range evs {
		if e.Project == "" || e.TS.Before(r.From) || !e.TS.Before(r.To) || !e.Counts() {
			continue
		}
		var n *int
		switch e.Kind {
		case event.Prompt:
			n = &get(e.TS, e.Project).Prompts
		case event.Edit:
			n = &get(e.TS, e.Project).Edits
		case event.Commit:
			n = &get(e.TS, e.Project).Commits
		case event.Run:
			n = &get(e.TS, e.Project).Runs
		default:
			continue
		}
		*n++
		addHarness(get(e.TS, e.Project), e.Harness)
	}
	// a prompt's model is its heartbeat's: the model that answered it
	for _, h := range r.Heartbeats {
		if h.Prompt && h.Model != "" && h.Project != "" && h.Project != OtherProject && !h.Time.Before(r.From) && h.Time.Before(r.To) {
			if b := m[key{h.Time.Unix() / 60, h.Project}]; b != nil && b.Prompts > 0 {
				b.addModel(h.Model, 0, 1)
			}
		}
	}
	out := make([]Bucket, 0, len(m))
	for _, b := range m {
		slices.Sort(b.Harnesses)
		slices.SortFunc(b.Models, func(x, y ModelUse) int { return cmp.Compare(x.Model, y.Model) })
		out = append(out, *b)
	}
	slices.SortFunc(out, func(a, b Bucket) int {
		return cmp.Or(a.Start.Compare(b.Start), cmp.Compare(a.Project, b.Project))
	})
	return out
}

// Sum adds buckets up, for one project or for all ("").
func Sum(bs []Bucket, project string) Bucket {
	t := Bucket{Project: project}
	for _, b := range bs {
		if project != "" && b.Project != project {
			continue
		}
		t.CodingSeconds += b.CodingSeconds
		t.AISeconds += b.AISeconds
		t.AINotCountedSeconds += b.AINotCountedSeconds
		t.Prompts += b.Prompts
		t.Edits += b.Edits
		t.Commits += b.Commits
		t.Runs += b.Runs
	}
	return t
}

// Hours rounds counted seconds to the tenth of an hour shown everywhere.
func Hours(sec float64) float64 { return float64(int64(sec/360+0.5)) / 10 }

// HackatimeHeartbeat is a heartbeat under Hackatime's own field names (its
// HEARTBEAT_KEYS), plus New: what `seatime inspect --heartbeats` prints.
type HackatimeHeartbeat struct {
	Time      float64 `json:"time"` // unix seconds
	Project   string  `json:"project"`
	Category  string  `json:"category"` // "coding" or "ai coding"
	Editor    string  `json:"editor,omitempty"`
	AISession string  `json:"ai_session,omitempty"`
	// AIPromptLength is the prompt's length in words: SeaTime keeps no
	// character count.
	AIPromptLength int   `json:"ai_prompt_length,omitempty"`
	New            *bool `json:"new,omitempty"` // on prompts only
}

// Hackatime is h under Hackatime's field names, with project as given.
func (h Heartbeat) Hackatime(project string) HackatimeHeartbeat {
	x := HackatimeHeartbeat{Time: float64(h.Time.UnixMilli()) / 1000, Project: project, Category: h.Category(),
		Editor: strings.TrimPrefix(h.Tool, "editor:"), AISession: h.Session}
	if h.Prompt {
		yours := h.Yours
		x.AIPromptLength, x.New = h.Words, &yours
	}
	return x
}
