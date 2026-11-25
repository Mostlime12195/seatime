package ledger

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"seatime/pkg/event"

	_ "modernc.org/sqlite"
)

var day = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

// hm is a time on the test day.
func hm(h, m int) time.Time {
	return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

// log builds an event log in order, as the store would.
type log []event.Event

func (l *log) add(e event.Event) { e.Seq = int64(len(*l) + 1); *l = append(*l, e) }

func (l *log) edit(proj string, t time.Time) {
	l.add(event.Event{Kind: event.Edit, Harness: "editor:vscode", Project: proj, TS: t, Category: Coding})
}

func (l *log) aiEdit(proj string, t time.Time) {
	l.add(event.Event{Kind: event.Edit, Harness: "editor:vscode", Project: proj, TS: t, Category: AICoding})
}

func (l *log) prompt(sess, proj string, t time.Time, text string) {
	l.add(event.Event{Kind: event.Prompt, Harness: "claude-code", SessionID: sess, Project: proj, TS: t, Text: text})
}

func (l *log) stop(sess, proj string, t time.Time) {
	l.add(event.Event{Kind: event.AgentStop, Harness: "claude-code", SessionID: sess, Project: proj, TS: t})
}

// count runs the rule over the whole test day.
func count(l log) Result { return Compute(l, day, day.Add(24*time.Hour)) }

func wantMin(t *testing.T, what string, gotSec, wantMin float64) {
	t.Helper()
	if math.Abs(gotSec-wantMin*60) > 1e-6 {
		t.Errorf("%s: %.2f min, want %.2f min", what, gotSec/60, wantMin)
	}
}

// Every case below, as a log and its counted minutes, so that
// TestIdleNeverChangesHours can rerun them all.
type example struct {
	name string
	log  func() log
	min  float64
}

var examples = []example{
	{"hand edits", handEdits, 3},
	{"one turn", oneTurn, 10},
	{"steering two hours", steering, 108},
	{"long run", longRun, 12},
	{"short replies", shortReplies, 10},
	{"repeat", repeat, 10},
	{"coaster", coaster, 34},
	{"agent edits", func() log { return agentEdits("github.com/kid/a") }, 10},
	{"plugin-marked AI", func() log { return pluginAI(true) }, 10},
	{"before school", beforeSchool, 4},
	{"five projects", fiveProjects, 6},
	{"no stop hook", noStop, 12},
}

const A, B, C, D, E = "github.com/kid/a", "github.com/kid/b", "github.com/kid/c", "github.com/kid/d", "github.com/kid/e"

func handEdits() log {
	var l log
	l.edit(A, hm(10, 0))
	l.edit(A, hm(10, 1))
	l.edit(A, hm(10, 5))
	return l
}

// Heartbeats at 10:00, 10:01 and 10:05: gaps of 1 and 4 min, the 4 capped
// at 2. 1 + 2 = 3 min, as on Hackatime.
func TestHandEdits(t *testing.T) {
	r := count(handEdits())
	wantMin(t, "total", r.Total, 3)
	wantMin(t, "hand", r.Coding, 3)
}

func oneTurn() log {
	var l log
	l.prompt("s", A, hm(10, 0), "add a login form to the settings page")
	l.stop("s", A, hm(10, 8))
	l.prompt("s", A, hm(10, 12), "now write tests for the login form")
	return l
}

// New prompt 10:00; the agent's heartbeats 10:01 ... 10:07 and its stop at
// 10:08, all within 10 min of the prompt: 8 gaps of 1 min, AI. New prompt
// at 10:12: a 4 min gap capped at 2, also AI (time spent on a prompt is AI
// work). 8 + 2 = 10 min, all AI.
func TestOneTurn(t *testing.T) {
	r := count(oneTurn())
	wantMin(t, "total", r.Total, 10)
	wantMin(t, "AI", r.AI, 10)
	wantMin(t, "hand", r.Coding, 0)
}

func steering() log {
	var l log
	for i := range 10 {
		at := hm(10, 12*i)
		l.prompt("s", A, at, fmt.Sprintf("now do step %d of the plan", i+1))
		l.stop("s", A, at.Add(9*time.Minute))
	}
	return l
}

// A new prompt every 12 min from 10:00 (10 prompts, the last at 11:48),
// each run 9 min. Each run is 9 min of AI inside its prompt's 10 min; each
// of the 9 gaps from a stop to the next prompt is 3 min, capped at 2.
// 9 × 10 + 2 × 9 = 108 min (the 9 × 11 + 9 of docs/RULE.md), all AI.
func TestSteeringTwoHours(t *testing.T) {
	r := count(steering())
	wantMin(t, "total", r.Total, 108)
	wantMin(t, "AI", r.AI, 108)
	wantMin(t, "AI not counted", r.AINotCounted, 0)
}

func longRun() log {
	var l log
	l.prompt("s", A, hm(10, 0), "build the whole parser module please")
	l.stop("s", A, hm(11, 0))
	l.prompt("s", A, hm(11, 0), "now add error messages to it")
	return l
}

// New prompt 10:00, the agent runs to 11:00, new prompt 11:00. Counted:
// the agent's 10:01 ... 10:10 (10 min), then 10:10 to the 11:00 prompt,
// capped at 2. 10 + 2 = 12 min. Not counted: the agent's 10:11 ... 10:59
// and its stop at 11:00 (which comes before the prompt at the same
// minute), 50 heartbeats a minute apart: 50 min.
func TestLongRunStopsCounting(t *testing.T) {
	r := count(longRun())
	wantMin(t, "total", r.Total, 12)
	wantMin(t, "AI not counted", r.AINotCounted, 50)
}

func shortReplies() log {
	var l log
	l.prompt("s", A, hm(10, 0), "refactor the upload handler to stream")
	l.prompt("s", A, hm(10, 4), "yes")
	l.prompt("s", A, hm(10, 8), "continue")
	l.prompt("s", A, hm(10, 12), "ok do it")
	l.stop("s", A, hm(10, 15))
	return l
}

// New prompt 10:00; "yes" 10:04, "continue" 10:08 and "ok do it" 10:12
// have fewer than 4 words, so they are the agent's heartbeats; the agent
// works until 10:15. Counted: 10:00 to 10:10 = 10 min. Not counted: 10:11
// to 10:15 = 5 min.
func TestShortRepliesOpenNothing(t *testing.T) {
	r := count(shortReplies())
	wantMin(t, "total", r.Total, 10)
	wantMin(t, "AI not counted", r.AINotCounted, 5)
}

func repeat() log {
	var l log
	l.prompt("s", A, hm(10, 0), "it still does not work")
	l.prompt("s", A, hm(10, 20), "It still does not work!")
	l.stop("s", A, hm(10, 30))
	return l
}

// "it still does not work" at 10:00 with a run to 10:30; "It still does
// not work!" at 10:20 is the same prompt once capitals and punctuation are
// dropped, so it opens nothing: 10:00 to 10:10 = 10 min. The same words in
// another project at 10:20 are new there: B's prompt is yours, and its gap
// from A's last counted heartbeat (10:10) is capped at 2. 10 + 2 = 12 min.
func TestRepeatOpensNothing(t *testing.T) {
	wantMin(t, "same project", count(repeat()).Total, 10)
	var l log
	l.prompt("s", A, hm(10, 0), "it still does not work")
	l.prompt("s2", B, hm(10, 20), "It still does not work!")
	l.stop("s", A, hm(10, 30))
	r := count(l)
	wantMin(t, "other project", r.Total, 12)
	wantMin(t, "other project, B", r.ByProject[B], 2)
}

// "make it blue" has 3 words and is not new; "make it blue please" is.
func TestFourWords(t *testing.T) {
	if NewPrompt("make it blue", false) || !NewPrompt("make it blue please", false) {
		t.Fatal("4 words is the line")
	}
	if NewPrompt("make it blue please", true) {
		t.Fatal("a prompt sent before is not new")
	}
	var l log
	l.prompt("s", A, hm(10, 0), "make it blue")
	l.prompt("s", B, hm(10, 0), "make it blue please")
	for _, h := range count(l).Heartbeats {
		if want := h.Project == B; h.Yours != want {
			t.Errorf("%s: yours=%v", h.Project, h.Yours)
		}
	}
}

func coaster() log {
	var l log
	for i, h := range []int{10, 11, 12} {
		l.prompt("s", A, hm(h, 0), fmt.Sprintf("long task number %d for the agent", i))
		l.stop("s", A, hm(h, 55))
	}
	return l
}

// New prompts 10:00, 11:00 and 12:00, each run 55 min. Each run counts its
// first 10 min; the gap from 10:10 to 11:00 and from 11:10 to 12:00 is
// capped at 2. 10 + 12 + 12 = 34 min. Not counted: 45 min of each run
// (10:11 ... 10:55), 3 × 45 = 135 min.
func TestCoaster(t *testing.T) {
	r := count(coaster())
	wantMin(t, "total", r.Total, 34)
	wantMin(t, "AI not counted", r.AINotCounted, 135)
}

func agentEdits(editProj string) log {
	var l log
	l.prompt("s", A, hm(10, 0), "rewrite the chart component in svg")
	for m := 0; m <= 30; m++ {
		l.edit(editProj, hm(10, m))
	}
	l.stop("s", A, hm(10, 30))
	return l
}

// New prompt 10:00, the agent runs to 10:30, an editor heartbeat every
// minute throughout. The editor heartbeats fall inside A's run, so they
// are the agent's: 10:00 to 10:10 = 10 min. If they are in project B
// instead, they are yours: A's agent counts 10:01 ... 10:10 and B's edits
// every minute after, 10 + 20 = 30 min.
func TestAgentEditsAreTheAgents(t *testing.T) {
	wantMin(t, "same project", count(agentEdits(A)).Total, 10)
	r := count(agentEdits(B))
	wantMin(t, "other project", r.Total, 30)
	wantMin(t, "other project, A", r.ByProject[A], 10)
	wantMin(t, "other project, B", r.ByProject[B], 20)
}

func pluginAI(handEdit bool) log {
	var l log
	for m := 0; m <= 30; m++ {
		l.aiEdit(A, hm(10, m))
	}
	if handEdit {
		l.edit(A, hm(10, 0))
	}
	return l
}

// Hand edit 10:00, then an "ai coding" heartbeat every minute to 10:30,
// with no prompt and no run known. The ai heartbeats count to 10:10:
// 10 min; 10:11 ... 10:30 do not: 20 min. Without the hand edit: 0 min,
// and the 30 gaps from 10:00 to 10:30 are not counted: 30 min.
func TestPluginMarkedAIIsTheAgents(t *testing.T) {
	r := count(pluginAI(true))
	wantMin(t, "total", r.Total, 10)
	wantMin(t, "AI not counted", r.AINotCounted, 20)
	r = count(pluginAI(false))
	wantMin(t, "without the hand edit", r.Total, 0)
	wantMin(t, "without the hand edit, not counted", r.AINotCounted, 30)
}

func beforeSchool() log {
	var l log
	l.prompt("s", A, hm(7, 40), "fix the typo in the header")
	l.stop("s", A, hm(7, 43))
	l.edit(A, hm(7, 44))
	return l
}

// New prompt 07:40, the agent to 07:43, hand edit 07:44. 3 + 1 = 4 min.
func TestBeforeSchool(t *testing.T) {
	r := count(beforeSchool())
	wantMin(t, "total", r.Total, 4)
	wantMin(t, "AI", r.AI, 3)
}

func fiveProjects() log {
	var l log
	for _, x := range []struct {
		p string
		m int
	}{{A, 0}, {B, 1}, {C, 2}, {D, 5}, {E, 6}, {A, 7}} {
		l.edit(x.p, hm(14, x.m))
	}
	return l
}

// Hand edits in A 14:00, B 14:01, C 14:02, D 14:05, E 14:06, A 14:07. Each
// gap goes to the project that ends it: B 1, C 1, D 3 capped at 2, E 1,
// A 1. Total 6 min, the sum of the five.
func TestFiveProjects(t *testing.T) {
	r := count(fiveProjects())
	for p, m := range map[string]float64{A: 1, B: 1, C: 1, D: 2, E: 1} {
		wantMin(t, p, r.ByProject[p], m)
	}
	wantMin(t, "total", r.Total, 6)
}

// A submission of A reveals only A's records (14:00, 14:07) and a bare time
// for each counted heartbeat elsewhere: 14:01, 14:02, 14:05, 14:06. The gap
// ending at 14:07 is then 1 min, as on the device; without the markers it
// would be 7 capped at 2.
func TestMarkersSplitGaps(t *testing.T) {
	var onlyA log
	for _, e := range fiveProjects() {
		if e.Project == A {
			onlyA = append(onlyA, e)
		}
	}
	wantMin(t, "without markers", count(onlyA).ByProject[A], 2)
	hbs := Mark(Heartbeats(onlyA), []time.Time{hm(14, 1), hm(14, 2), hm(14, 5), hm(14, 6)})
	r := Count(hbs, day, day.Add(24*time.Hour))
	wantMin(t, "with markers", r.ByProject[A], 1)
	wantMin(t, "with markers, total", r.Total, 1)
}

// The TestHandEdits heartbeats from two devices (each with its own seq
// numbers): 1 + 2 = 3 min.
func TestTwoDevices(t *testing.T) {
	l := handEdits()
	l[1].Harness, l[1].Seq = "editor:zed", 1
	wantMin(t, "total", count(l).Total, 3)
}

// The TestHandEdits events given backwards: 3 min. Heartbeats at 10:00 and
// 12:00 with the lid shut in between (no idle records): one gap of 2 h,
// capped at 2 min.
func TestOrderAndSleep(t *testing.T) {
	l := handEdits()
	slices.Reverse(l)
	wantMin(t, "backwards", count(l).Total, 3)
	var s log
	s.edit(A, hm(10, 0))
	s.edit(A, hm(12, 0))
	wantMin(t, "sleep", count(s).Total, 2)
}

func noStop() log {
	var l log
	l.prompt("s", A, hm(10, 0), "set up the database migrations")
	l.prompt("s", A, hm(15, 0), "add an index to the users table")
	return l
}

// New prompt 10:00, no stop ever; the run ends at the session's next
// event, the new prompt at 15:00. 10:00 to 10:10, then 10:10 to 15:00
// capped at 2. 10 + 2 = 12 min.
func TestNoStopHook(t *testing.T) {
	wantMin(t, "total", count(noStop()).Total, 12)
}

// Every example above with an idle record saying no input all day: the
// same minutes. Under this rule the idle timer never changes a number.
func TestIdleNeverChangesHours(t *testing.T) {
	for _, ex := range examples {
		l := ex.log()
		for h := range 24 {
			end := day.Add(time.Duration(h+1) * time.Hour)
			l.add(event.Event{Kind: event.Idle, Harness: "seatime", TS: end, IdleSrc: "ioreg", WindowMS: time.Hour.Milliseconds(),
				NoInput: [][2]int64{{end.Add(-time.Hour).UnixMilli(), end.UnixMilli()}}})
		}
		wantMin(t, ex.name, count(l).Total, ex.min)
		wantMin(t, ex.name+" without idle", count(ex.log()).Total, ex.min)
	}
}

// The known limits in docs/RULE.md, pinned at what they pay, so nobody
// closes one with a quiet clause.
func TestKnownLimits(t *testing.T) {
	// A script that sends a fake heartbeat every 2 min from 10:00 to 16:00:
	// 180 gaps of 2 min = 360 min, 6 h, exactly as on Hackatime.
	var fake log
	for m := 0; m <= 360; m += 2 {
		fake.edit(A, hm(10, m))
	}
	wantMin(t, "fake heartbeats", count(fake).Total, 360)

	// A fresh line after each 14-minute run, every 15 min, 8 times from
	// 10:00: each run counts 10 min and the gap from 10 min to the next
	// prompt is 5, capped at 2, so 12 of every 15; the last run has no next
	// prompt. 7 × 12 + 10 = 94 min.
	var fresh log
	for i := range 8 {
		at := hm(10, 15*i)
		fresh.prompt("s", A, at, fmt.Sprintf("please keep going with part %d", i))
		fresh.stop("s", A, at.Add(14*time.Minute))
	}
	wantMin(t, "fresh line every 15 min", count(fresh).Total, 94)

	// A fresh prompt every 10 min with the agent always working, from 10:00
	// to a stop at 11:00: paid in full, 60 min.
	var feeder log
	for i := range 6 {
		feeder.prompt("s", A, hm(10, 10*i), fmt.Sprintf("another fresh idea number %d", i))
	}
	feeder.stop("s", A, hm(11, 0))
	wantMin(t, "fresh prompt every 10 min", count(feeder).Total, 60)

	// A real prompt, then walking away while the agent runs an hour: 10 min;
	// coming back with a new prompt adds the capped gap, 12 min (longRun).
	var away log
	away.prompt("s", A, hm(10, 0), "migrate the whole app to typescript")
	away.stop("s", A, hm(11, 0))
	wantMin(t, "walked away", count(away).Total, 10)
	wantMin(t, "walked away and came back", count(longRun()).Total, 12)

	// Thinking for 5 min between two hand edits: one gap, capped at 2.
	var think log
	think.edit(A, hm(10, 0))
	think.edit(A, hm(10, 5))
	wantMin(t, "thinking", count(think).Total, 2)
}

// Adjacent ranges add up to the whole: the TestSteeringTwoHours day cut at
// 10:30 (inside a run) and at noon.
func TestRangesAddUp(t *testing.T) {
	l := steering()
	var sum float64
	for _, cut := range [][2]time.Time{{day, hm(10, 30)}, {hm(10, 30), hm(12, 0)}, {hm(12, 0), day.Add(24 * time.Hour)}} {
		sum += Compute(l, cut[0], cut[1]).Total
	}
	wantMin(t, "sum of three ranges", sum, 108)
}

// Hackatime's own SQL, run in SQLite over five fixture days of "coding"
// heartbeats, gives the same seconds as Compute, to the second.
//
// The two queries are with_capped_duration over duration_seconds (the
// ungrouped total) and attributed_durations_by(scope, :project), from
// app/models/concerns/heartbeatable.rb in github.com/hackclub/hackatime
// (MIT License, Copyright (c) 2026 Hack Club). Postgres's LEAST is SQLite's
// two-argument MIN; nothing else changed.
//
// Each day starts at 09:00 and is 10 rounds of these gaps between
// heartbeats, in seconds: 1, 30, 59, 60, 61, 119, 120, 121, 300, 3600, 0,
// 45 (4,516 s a round, so a day ends at 21:33). Capped at 120 one round is
// 1 + 30 + 59 + 60 + 61 + 119 + 120 + 120 + 120 + 120 + 0 + 45 = 855 s.
// 120 heartbeats a day have 119 gaps (the last round lacks its 45):
// 855 × 10 − 45 = 8,505 s. The four gaps from one day's last heartbeat to
// the next day's first are capped at 120: 5 × 8,505 + 4 × 120 = 43,005 s.
// Heartbeats rotate over three projects; a 0 s gap is a tie, ordered by id.
func TestSameAsHackatime(t *testing.T) {
	gaps := []int{1, 30, 59, 60, 61, 119, 120, 121, 300, 3600, 0, 45}
	projects := []string{A, B, C}
	var l log
	for d := range 5 {
		at := day.Add(time.Duration(d)*24*time.Hour + 9*time.Hour)
		for i := range 120 {
			if i > 0 {
				at = at.Add(time.Duration(gaps[(i-1)%len(gaps)]) * time.Second)
			}
			l.edit(projects[(i*7+d)%3], at)
		}
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE heartbeats (id INTEGER PRIMARY KEY, user_id INTEGER, time REAL, project TEXT, category TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, e := range l {
		if _, err := db.Exec(`INSERT INTO heartbeats VALUES (?, 1, ?, ?, 'coding')`, e.Seq, float64(e.TS.Unix()), e.Project); err != nil {
			t.Fatal(err)
		}
	}
	const timeout = 120 // heartbeat_timeout_duration
	capped := func(intervals string) string {
		return `SELECT heartbeat_intervals.*, CASE WHEN heartbeat_intervals.duration_start IS NULL OR heartbeat_intervals.duration_end IS NULL THEN 0
		     ELSE MIN(heartbeat_intervals.duration_end - heartbeat_intervals.duration_start, ` + strconv.Itoa(timeout) + `) END AS duration
		     FROM (` + intervals + `) AS heartbeat_intervals`
	}
	var total float64
	totalSQL := `SELECT COALESCE(SUM(duration), 0) FROM (` + capped(`SELECT LAG(time) OVER (ORDER BY time, heartbeats.id) as duration_start, time as duration_end FROM heartbeats WHERE time IS NOT NULL`) + `) AS durations`
	if err := db.QueryRow(totalSQL).Scan(&total); err != nil {
		t.Fatal(err)
	}
	byProjectSQL := `SELECT bucket, COALESCE(SUM(duration), 0) AS duration FROM (` + capped(`SELECT attribution_heartbeats.project AS bucket,
		LAG(attribution_heartbeats.time) OVER (ORDER BY attribution_heartbeats.time, attribution_heartbeats.id) AS duration_start,
		attribution_heartbeats.time AS duration_end FROM (SELECT id, time, project FROM heartbeats) AS attribution_heartbeats`) + `) attributed_durations
		WHERE bucket IS NOT NULL AND bucket <> '' GROUP BY bucket`
	rows, err := db.Query(byProjectSQL)
	if err != nil {
		t.Fatal(err)
	}
	hack := map[string]float64{}
	for rows.Next() {
		var p string
		var s float64
		if err := rows.Scan(&p, &s); err != nil {
			t.Fatal(err)
		}
		hack[p] = s
	}
	r := Compute(l, day, day.Add(6*24*time.Hour))
	if total != 43005 || r.Total != total {
		t.Fatalf("total: Hackatime %.0f s, Compute %.0f s, by hand 43005 s", total, r.Total)
	}
	for _, p := range projects {
		if r.ByProject[p] != hack[p] {
			t.Errorf("%s: Hackatime %.0f s, Compute %.0f s", p, hack[p], r.ByProject[p])
		}
	}
}

// docs/RULE.md names the three numbers; they must be the code's.
func TestRuleNumbersMatchDoc(t *testing.T) {
	b, err := os.ReadFile("../../docs/RULE.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`The rule has three numbers: (\d+) minutes \(from Hackatime\), (\d+) minutes and (\d+) words`).FindSubmatch(b)
	if m == nil {
		t.Fatal(`docs/RULE.md has no "The rule has three numbers: ..." line`)
	}
	if !strings.Contains(string(b), Rule) {
		t.Error("ledger.Rule is not the paragraph in docs/RULE.md")
	}
	got := fmt.Sprintf("%s %s %s", m[1], m[2], m[3])
	want := fmt.Sprintf("%d %d %d", int(Timeout.Minutes()), int(Window.Minutes()), MinWords)
	if got != want {
		t.Fatalf("docs/RULE.md says %s, the code %s", got, want)
	}
}
