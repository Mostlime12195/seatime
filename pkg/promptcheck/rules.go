package promptcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// Rules is one versioned set of what the check asks and how it decides:
// the questions, which answers count (Effort, Block, Need), where a
// probability becomes a yes, the kinds of prompt with their minutes, and
// the caps on what one check may carry. The server keeps it as a setting
// (internal/server, setting prompt_check_rules), so a re-tune is an edit,
// not a release. Every verdict records the rules it was judged with
// (Verdict.Rules) and the minutes it was given (Verdict.Minutes): a device
// counts with the minutes the server sent, never with a table of its own,
// and an old verdict keeps its minutes when the rules change.
type Rules struct {
	// Version names the set ("v3"). ID adds a hash of the content, so an
	// edit that forgot to change the version is still told apart.
	Version   string            `json:"version"`
	Questions map[string]string `json:"questions"`
	// Effort: a prompt counts only with a yes to one of these.
	Effort []string `json:"effort"`
	// Block: a yes to any of these keeps a prompt from counting.
	Block []string `json:"block"`
	// Need: a prompt counts only with a yes to each of these.
	Need []string `json:"need"`
	// Unbacked is the question asked only when the device measured the time
	// the AI tool's window was in front ("" for none). Not asked, it is
	// stored as not asked (null) and decides as no.
	Unbacked string `json:"unbacked,omitempty"`
	// Yes is where a probability becomes a yes.
	Yes float64 `json:"yes"`
	// Kinds are the kinds of prompt in order: a prompt is the first kind
	// whose Label it has a yes to. The last kind has no label: everything
	// else.
	Kinds []KindRule `json:"kinds"`
	Caps  Caps       `json:"caps"`
}

// KindRule is one kind of prompt: its name, the question that marks it,
// and how many minutes a passing prompt of that kind may count.
type KindRule struct {
	Name    string  `json:"name"`
	Label   string  `json:"label,omitempty"`
	Minutes float64 `json:"minutes"`
}

// Caps bound one check request, in bytes (ContextTurns in turns). The
// server cuts what goes past them before the model reads it; only a
// request that is not a check's shape is refused.
type Caps struct {
	Prompt       int `json:"prompt"`
	Reply        int `json:"reply"`
	Diff         int `json:"diff"`
	Activity     int `json:"activity"`
	ContextField int `json:"context_field"`
	ContextTurns int `json:"context_turns"`
	// Earlier is the cap on each earlier prompt (0: 1024).
	Earlier int `json:"earlier,omitempty"`
	// Tree and Readme are the caps on the project's file names and on its
	// README (0: MaxTree, MaxReadme).
	Tree   int `json:"tree,omitempty"`
	Readme int `json:"readme,omitempty"`
}

// TreeCap is the cap on the project's file names, in bytes.
func (c Caps) TreeCap() int {
	if c.Tree <= 0 {
		return MaxTree
	}
	return c.Tree
}

// ReadmeCap is the cap on the project's README, in bytes.
func (c Caps) ReadmeCap() int {
	if c.Readme <= 0 {
		return MaxReadme
	}
	return c.Readme
}

// EarlierCap is the cap on each earlier prompt shown, in bytes.
func (c Caps) EarlierCap() int {
	if c.Earlier <= 0 {
		return 1024
	}
	return c.Earlier
}

// DefaultRules is the compiled default: one plain question and one kind of
// prompt, enough for a server to run the check as it comes. A server that
// wants its own questions sets them in its configuration (internal/server,
// setting prompt_check_rules), where they stay private.
func DefaultRules() *Rules {
	return &Rules{
		Version: "open-1",
		Questions: map[string]string{
			"own_input": "Does the prompt add something of the person's own to the project they are building: a new detail, a choice, a reaction to the AI's last result, or a question to learn something? A bare go-ahead ('yes', 'continue', 'fix it') and a request that an earlier prompt already made do not count.",
		},
		Effort: []string{"own_input"},
		Yes:    0.5,
		Kinds:  []KindRule{{Name: "prompt", Minutes: FallbackMinutes.Minutes()}},
		Caps:   Caps{Prompt: MaxPrompt, Reply: MaxReply, Diff: MaxDiff, Activity: MaxActivity, ContextField: MaxContextField, ContextTurns: MaxContextTurns, Tree: MaxTree, Readme: MaxReadme},
	}
}

// ParseRules reads rules from JSON and checks them (Validate).
func ParseRules(b []byte) (*Rules, error) {
	var r Rules
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Hard bounds on the caps: the server reads at most this much of a request.
const (
	hardMaxField = 64 << 10
	hardMaxTurns = 20
)

// Validate refuses rules that cannot decide: an answer group naming a
// question that is not asked, no fallback kind, minutes or caps out of
// range.
func (r *Rules) Validate() error {
	if r == nil {
		return errors.New("rules: empty")
	}
	if r.Version == "" || len(r.Version) > 40 {
		return errors.New("rules: version must be 1 to 40 characters")
	}
	if len(r.Questions) == 0 {
		return errors.New("rules: no questions")
	}
	for k, q := range r.Questions {
		if k == "" || len(k) > 40 || q == "" {
			return fmt.Errorf("rules: question %q is empty or its name is too long", k)
		}
	}
	has := func(k string) bool { _, ok := r.Questions[k]; return ok }
	for name, g := range map[string][]string{"effort": r.Effort, "block": r.Block, "need": r.Need} {
		for _, k := range g {
			if !has(k) {
				return fmt.Errorf("rules: %s names %q, which is not a question", name, k)
			}
		}
	}
	if len(r.Effort) == 0 {
		return errors.New("rules: effort is empty, so no prompt could count")
	}
	if r.Unbacked != "" && !has(r.Unbacked) {
		return fmt.Errorf("rules: unbacked names %q, which is not a question", r.Unbacked)
	}
	if !(r.Yes > 0 && r.Yes < 1) {
		return errors.New("rules: yes must be between 0 and 1")
	}
	if len(r.Kinds) == 0 {
		return errors.New("rules: no kinds")
	}
	seen := map[string]bool{}
	for i, k := range r.Kinds {
		last := i == len(r.Kinds)-1
		switch {
		case k.Name == "" || seen[k.Name]:
			return fmt.Errorf("rules: kind %d has no name or a repeated one", i+1)
		case last && k.Label != "":
			return errors.New("rules: the last kind is the fallback and takes no label")
		case !last && !has(k.Label):
			return fmt.Errorf("rules: kind %q is marked by %q, which is not a question", k.Name, k.Label)
		case !(k.Minutes > 0 && k.Minutes <= 60) || math.IsNaN(k.Minutes):
			return fmt.Errorf("rules: kind %q must count for more than 0 and at most 60 minutes", k.Name)
		}
		seen[k.Name] = true
	}
	c := r.Caps
	for name, v := range map[string]int{"prompt": c.Prompt, "reply": c.Reply, "diff": c.Diff, "activity": c.Activity, "context_field": c.ContextField} {
		if v < 64 || v > hardMaxField {
			return fmt.Errorf("rules: caps.%s must be 64 to %d bytes", name, hardMaxField)
		}
	}
	for name, v := range map[string]int{"earlier": c.Earlier, "tree": c.Tree, "readme": c.Readme} {
		if v < 0 || v > hardMaxField {
			return fmt.Errorf("rules: caps.%s must be 0 to %d bytes", name, hardMaxField)
		}
	}
	if c.ContextTurns < 0 || c.ContextTurns > hardMaxTurns {
		return fmt.Errorf("rules: caps.context_turns must be 0 to %d", hardMaxTurns)
	}
	return nil
}

// JSON is the rules as stored, indented.
func (r *Rules) JSON() []byte {
	b, _ := json.MarshalIndent(r, "", "  ")
	return b
}

// ID is the version plus the first bytes of a hash of the content
// ("v3-3fa2c1d0"): what each verdict records.
func (r *Rules) ID() string {
	b, _ := json.Marshal(r) // map keys are sorted: the hash is stable
	h := sha256.Sum256(b)
	return r.Version + "-" + hex.EncodeToString(h[:4])
}

// Keys are the question names, sorted.
func (r *Rules) Keys() []string {
	ks := make([]string, 0, len(r.Questions))
	for k := range r.Questions {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Ask is every question about prompt n only, but the skipped ones.
func (r *Rules) Ask(n int, skip ...string) map[string]Question {
	qs := make(map[string]Question, len(r.Questions))
	for k, v := range r.Questions {
		if slices.Contains(skip, k) {
			continue
		}
		qs[k] = Question{Type: "noul", Instructions: fmt.Sprintf("About PROMPT %d only: %s%s", n, v, Guard)}
	}
	return qs
}

// LabelsOf turns probabilities into labels; a question the model did not
// answer is missing.
func (r *Rules) LabelsOf(p map[string]float64) Labels {
	l := Labels{}
	for k := range r.Questions {
		if v, ok := p[k]; ok {
			l[k] = v >= r.Yes
		}
	}
	return l
}

// Complete reports whether every question asked has an answer.
func (r *Rules) Complete(l Labels, skipped ...string) bool {
	for k := range r.Questions {
		if _, ok := l[k]; !ok && !slices.Contains(skipped, k) {
			return false
		}
	}
	return true
}

// Pass is the decision: a yes to one of Effort, a no to every Block and
// a yes to each Need. A skipped question decides as no; any other missing
// answer counts against the prompt.
func (r *Rules) Pass(l Labels, skipped ...string) bool {
	for _, k := range r.Block {
		v, ok := l[k]
		if !ok && !slices.Contains(skipped, k) || v {
			return false
		}
	}
	for _, k := range r.Need {
		if !l[k] {
			return false
		}
	}
	for _, k := range r.Effort {
		if l[k] {
			return true
		}
	}
	return false
}

// Kind is the first kind whose label has a yes, else the last kind.
func (r *Rules) Kind(l Labels) string {
	for _, k := range r.Kinds {
		if k.Label == "" || l[k.Label] {
			return k.Name
		}
	}
	return r.Kinds[len(r.Kinds)-1].Name
}

// MinutesOf is how long a passing prompt of kind may count (the fallback
// kind's minutes for a kind these rules do not have).
func (r *Rules) MinutesOf(kind string) float64 {
	for _, k := range r.Kinds {
		if k.Name == kind {
			return k.Minutes
		}
	}
	return r.Kinds[len(r.Kinds)-1].Minutes
}

// Longest is the most minutes any kind may count.
func (r *Rules) Longest() time.Duration {
	var m float64
	for _, k := range r.Kinds {
		m = max(m, k.Minutes)
	}
	return Mins(m)
}

// Verdict is the decision for labels under these rules, judged by model,
// with the questions not asked (skipped).
func (r *Rules) Verdict(l Labels, model string, skipped ...string) Verdict {
	v := Verdict{Labels: l, Pass: r.Pass(l, skipped...), Kind: r.Kind(l), Model: model, Rules: r.ID(), Skipped: skipped}
	if v.Pass {
		v.Minutes = r.MinutesOf(v.Kind)
	}
	return v
}

// Cut cuts each field of t to these caps; judged selects the caps of the
// turn being judged.
func (r *Rules) Cut(t Turn, judged bool) Turn {
	c := r.Caps
	p, rp, d := c.ContextField, c.ContextField, c.ContextField
	if judged {
		p, rp, d = c.Prompt, c.Reply, c.Diff
	}
	t.Prompt, _ = Cut(t.Prompt, p)
	t.PrevReply, _ = Cut(t.PrevReply, rp)
	t.Diff, _ = Cut(t.Diff, d)
	t.Activity, _ = Cut(t.Activity, c.Activity)
	return t
}

// CutProject cuts the project facts to these caps (nil stays nil): the
// file names on a line boundary, the rest on a rune boundary.
func (r *Rules) CutProject(p *Project) *Project {
	if p == nil {
		return nil
	}
	q := *p
	q.Name, _ = Cut(q.Name, MaxProjectName)
	if f, cut := Cut(q.Files, r.Caps.TreeCap()); cut {
		q.Files = f[:strings.LastIndexByte(f, '\n')+1]
	}
	q.Readme, _ = Cut(q.Readme, r.Caps.ReadmeCap())
	return &q
}

// Mins is a number of minutes as a duration.
func Mins(m float64) time.Duration { return time.Duration(m * float64(time.Minute)) }
