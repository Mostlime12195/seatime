package sim

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/emit"
	clientstore "seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

// Attacks returns the red-team personas. Work is the attacker's real human
// effort in hours (writing/running the script, being at the keyboard); the
// Effort note says how it was counted. Effort for scripts is the first
// faker's; a copycat who downloads a published script pays a fraction.
func Attacks() []*Persona {
	return append([]*Persona{
		{Name: "heartbeat_bot", Gate: true, Want: "succeeds", // heartbeat/1 known limit: fake heartbeats are counted as on Hackatime; the facts show it
			Desc: "Script posts WakaTime heartbeats to the local endpoint every 120 s for 6 h, one line of growth each, rotating files.",
			Run:  atkHeartbeatBot(false)},
		{Name: "jiggler_heartbeat_script", Gate: false, Desc: "heartbeat_bot_jitter plus a $5 mouse jiggler, so the OS idle timer sees input every minute (the client cannot tell a jiggler from a hand). Measured, not gated.",
			Run: atkJigglerScript},
		{Name: "heartbeat_bot_jitter", Gate: false, Desc: "Smarter heartbeat script: random 40-130 s gaps, saves, file switches, noisy line growth, 6 h, with a commit every hour.",
			Run: atkHeartbeatBot(true)},
		{Name: "afk_claude_p_loop", Gate: true, Want: "blocked", Desc: "`while true; do claude -p \"continue the TODO list\"; done` for 6 h. Hooks see CLAUDE_CODE_ENTRYPOINT=sdk-cli.",
			Run: atkAFKLoop(false)},
		{Name: "afk_self_reprompt", Gate: true, Want: "succeeds", // heartbeat/1: the repeated line is not new, so only its first 10 min count
			Desc: "Interactive Claude Code session that re-prompts itself from a Stop hook (\"continue with the next item\", 2-4 s after each stop) for 6 h; nobody at the keyboard. Env spoofed to look interactive.",
			Run:  atkAFKLoop(true)},
		{Name: "llm_fake_user", Gate: true, Want: "succeeds", // known residual: an LLM typing into a live desktop is not stopped by the client
			Desc: "An LLM writes varied, specific, novel-word prompts that react to the last run's output, on a human-like schedule (log-normal think time, breaks), driving a real agent that edits, tests and commits. 6 h/day.",
			Run:  atkLLMFakeUser},
		{Name: "prompt_stuffing", Gate: false, Desc: "A real beginner session with every prompt padded with rare technical words (\"toroidal\", \"quaternion\", \"idempotent\"). Compared with the same work written plainly.",
			Run: atkStuffing},
		{Name: "replay_old_session", Gate: true, Want: "discounted", // heartbeat/1: no prompt fingerprint leaves the device, so the server cannot match replayed prompts
			Desc: "Re-emits a previously submitted real session (react todo app) through `seatime emit` a week later, paced in real time, as a new project.",
			Run:  atkReplay},
		{Name: "backfill_synced_device", Gate: true, Want: "discounted", Desc: "Device synced in the morning; at 17:00 a script writes 6 h of fabricated activity timestamped 11:00-17:00 (client clock wound back), then submits.",
			Run: atkBackfill(false)},
		{Name: "backfill_offline", Gate: true, Want: "discounted", Desc: "Same, but the daemon was stopped all day so no checkpoint brackets the fake hours.",
			Run: atkBackfill(true)},
		{Name: "clock_forward", Gate: false, Want: "blocked", Desc: "Client clock 3 h fast during 1 h of real work (claims the hours happened later, e.g. inside a program window).",
			Run: atkClock(3 * hr)},
		{Name: "clock_back", Gate: false, Desc: "Client clock 2 h slow during 1 h of real work after the device had synced (claims the work happened earlier, e.g. before a deadline).",
			Run: atkClock(-2 * hr)},
		{Name: "clock_dilation", Gate: true, Want: "blocked", Desc: "Client clock starts 3 h behind and runs at 2x speed while syncing normally: 3 h of real work would claim 6 h.",
			Run: atkDilation},
		{Name: "two_device_double_count", Gate: true, Want: "discounted", Desc: "1.5 h of real work on the laptop; a script mirrors every event to a second enrolled device; both devices submit the project.",
			Run: atkTwoDeviceDouble},
		{Name: "split_projects", Gate: true, Want: "discounted", Desc: "One person, 2 h, rotates one prompt at a time through 4 repos; submits all 4 (each repo's gaps are the others' work).",
			Run: atkSplitProjects},
		{Name: "resubmit_double_dip", Gate: false, Desc: "Submits the same project twice (two programs) from the same device.",
			Run: atkResubmit},
		{Name: "fake_test_toggle", Gate: false, Desc: "Script loop for 4 h: 'fix the failing test' prompt, agent edit, a test that fails with a fresh error, next prompt, passes. Each cycle is an error->fix arc.",
			Run: atkTestToggle},
		{Name: "forged_commit_dates", Gate: false, Desc: "30 commits made in 30 min with GIT_COMMITTER_DATE spread over the past 6 days (the daemon's git scan trusts the committer date).",
			Run: atkForgedCommits},
		{Name: "bundle_tamper", Gate: true, Want: "blocked", Desc: "Four edited bundles: an event's timestamp moved, events dropped, two events swapped, project key swapped (all re-signed with the device key).",
			Run: atkBundleTamper},
		{Name: "hide_own_dark_flow", Gate: true, Want: "discounted", // hiding gains nothing: the unedited bundle scores the same (ledger/2)
			Desc: "Frantic dark-flow session; at submit time the attacker turns their own 'fix it' prompts and agent_stops into bare leaf hashes (allowed for other projects) and re-signs. (Score v4: what is left looks like a one-prompt session nobody was at, so it earns nothing: blocked, was discounted.)",
			Run:  atkHideOwn},
		{Name: "cherry_pick_other_project", Gate: false, Want: "blocked", Desc: "Relabels revealed events of another project as the submitted project in the bundle.",
			Run: atkCherryPick},
		{Name: "copy_bundle_or_key", Gate: true, Want: "blocked", Desc: "Y uploads X's bundle (as is; re-signed with Y's key); then Y enrolls a device with X's copied key pair and mirrors X's checkpoints live, and submits X's chain.",
			Run: atkCopy},
		{Name: "emit_injection", Gate: false, Desc: "`seatime emit` script fabricates a full session (prompts with forged similarity and extras, which the client strips, agent turns, writes, failing->passing runs) on a human-like schedule for 6 h; no agent exists.",
			Run: atkEmitInjection},
		{Name: "evil_web_page", Gate: true, Want: "blocked", Desc: "A web page the user visits POSTs fabricated prompts/heartbeats to 127.0.0.1:47823 (no secret, guessed secret, DNS-rebound Host, browser extension origin).",
			Run: atkEvilPage},
		{Name: "vm_farm_10", Gate: false, Desc: "Ten VMs (ten enrolled devices, one account), each running the self-reprompting agent loop for 6 h on its own repo; ten submissions.",
			Run: atkVMFarm},
		{Name: "rubber_stamp_hours", Gate: true, Want: "discounted", Desc: "Human at the keyboard for 4 h typing 'yes' / 'continue' / 'looks good' after every agent turn; the AI writes everything.",
			Run: atkRubberStamp},
		{Name: "alarm_clock_attendance", Gate: false, Desc: "Long autonomous agent turns (~14 min); the human comes back ~1 min after each stop, types a one-line 'keep going' and leaves. 6 h.",
			Run: atkAlarmClock},
		{Name: "forged_sync_buckets", Gate: true, Desc: "Bypasses the client: POSTs /api/v1/sync with fabricated minute buckets (60 s/min in 50 projects for 8 h of a past day). Measured on the Hackatime-compatible stats API.",
			Run: atkForgedBuckets},
	}, moreAttacks...)
}

// moreAttacks are personas other files of this package add (private
// catalogues kept out of the public repository register here).
var moreAttacks []*Persona

func outcome(subs ...*SubResult) *Outcome {
	o := &Outcome{Subs: subs, Compat: -1}
	for _, s := range subs {
		o.Credited += s.Hours()
		if s != nil {
			o.Presence += s.Scored.Seconds() / 3600
		}
	}
	return o
}

// ---------------------------------------------------------------- heartbeat bots

func atkHeartbeatBot(jitter bool) func(w *World, seed uint64) (*Outcome, error) {
	return func(w *World, seed uint64) (*Outcome, error) {
		d, err := w.NewDevice("beatbot", "laptop")
		if err != nil {
			return nil, err
		}
		a := NewActor(d, seed).In(d.Project("", "game"))
		a.NoHands = true // a script; nobody at the machine
		files := []string{"src/main.py", "src/player.py", "src/level.py", "src/ui.py"}
		end := w.Now().Add(6 * hr)
		n := 0
		for w.Now().Before(end) {
			f := files[(n/7)%len(files)]
			if !jitter {
				a.Wait(120 * sec)
				a.lines[f]++
				a.Heartbeat(f, false)
			} else {
				a.Wait(time.Duration(40+a.rng.IntN(90)) * sec)
				a.lines[f] += a.rng.IntN(3)
				a.Heartbeat(f, a.rng.IntN(4) == 0)
				if n%40 == 39 {
					a.Commit()
				}
			}
			n++
		}
		a.Wait(3 * mn)
		o := outcome(d.Submit(ProjectKey("beatbot", "game"), SubmitOpts{}))
		o.Compat = w.CompatHours("beatbot", "game")
		if jitter {
			o.Work, o.Copycat, o.Effort = 1.0, 0.1, "1 h to write a jittered heartbeat script (copycat: minutes)"
		} else {
			o.Work, o.Copycat, o.Effort = 0.5, 0.05, "30 min to write a 10-line loop (copycat: minutes)"
		}
		return o, nil
	}
}

// atkJigglerScript is the jittered heartbeat script with a mouse jiggler:
// the idle timer sees input, so rule 8 cannot remove the script's time.
func atkJigglerScript(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("jiggler", "laptop")
	if err != nil {
		return nil, err
	}
	a := NewActor(d, seed).In(d.Project("", "game"))
	a.NoHands = true
	files := []string{"src/main.py", "src/player.py", "src/level.py", "src/ui.py"}
	end := w.Now().Add(6 * hr)
	for n := 0; w.Now().Before(end); n++ {
		f := files[(n/7)%len(files)]
		a.Wait(time.Duration(40+a.rng.IntN(90)) * sec)
		d.Touch() // the jiggler
		a.lines[f] += a.rng.IntN(3)
		a.Heartbeat(f, a.rng.IntN(4) == 0)
		if n%40 == 39 {
			a.Commit()
		}
	}
	a.Wait(3 * mn)
	o := outcome(d.Submit(ProjectKey("jiggler", "game"), SubmitOpts{}))
	o.Work, o.Copycat, o.Effort = 1.0, 0.1, "1 h for the jittered script and $5 for a jiggler (copycat: minutes)"
	return o, nil
}

// ---------------------------------------------------------------- AFK agent loops

func atkAFKLoop(spoof bool) func(w *World, seed uint64) (*Outcome, error) {
	return func(w *World, seed uint64) (*Outcome, error) {
		d, err := w.NewDevice("afk", "laptop")
		if err != nil {
			return nil, err
		}
		root := d.Project("", "saas")
		a := NewActor(d, seed).In(root)
		a.NoHands = true
		if !spoof {
			os.Setenv("CLAUDE_CODE_ENTRYPOINT", "sdk-cli")
			defer os.Unsetenv("CLAUDE_CODE_ENTRYPOINT")
		}
		end := w.Now().Add(6 * hr)
		a.Start()
		first := true
		for i := 0; w.Now().Before(end); i++ {
			if !spoof && !first {
				a.In(root) // claude -p: a new session every iteration
				a.Start()
			}
			if first {
				a.Prompt(2*mn, "build a SaaS invoicing app from TODO.md: auth, clients, invoices, PDF export, stripe; work through the list one item at a time and run the tests after each")
				first = false
			} else {
				a.Wait(time.Duration(2+a.rng.IntN(3)) * sec)
				a.Send("continue with the next item in TODO.md")
			}
			steps := []Step{R(20 * sec)}
			for k := 0; k < 3+a.rng.IntN(4); k++ {
				steps = append(steps, W(time.Duration(30+a.rng.IntN(60))*sec, fmt.Sprintf("app/m%d.py", (i*3+k)%25)))
			}
			if a.rng.IntN(3) == 0 {
				steps = append(steps, Fail(40*sec, "pytest", fmt.Sprintf("E   AssertionError: invoice total %d != %d\nFAILED tests/test_inv.py::test_total_%d", 100+i, 99+i, i)))
				steps = append(steps, W(40*sec, fmt.Sprintf("app/m%d.py", i%25)))
			}
			steps = append(steps, Pass(40*sec, "pytest"))
			a.Agent(steps...)
			if i%5 == 4 {
				d.Commit(root, time.Time{}, 120, 4) // the agent commits
			}
			if !spoof {
				a.End()
			}
		}
		a.Wait(3 * mn)
		o := outcome(d.Submit(ProjectKey("afk", "saas"), SubmitOpts{}))
		if !spoof {
			o.Work, o.Effort = 0.1, "one shell loop"
			if o.Subs[0].Err != "" && strings.Contains(o.Subs[0].Err, "no events") {
				o.Blocked = true
				o.Notes = append(o.Notes, "headless runs were dropped by the Claude Code adapter; nothing was recorded")
			}
		} else {
			o.Work, o.Copycat, o.Effort = 0.25, 0.05, "15 min: a Stop hook that re-prompts + exporting CLAUDE_CODE_ENTRYPOINT=cli"
		}
		return o, nil
	}
}

// ---------------------------------------------------------------- LLM-driven fake user

var fakeTopics = []string{"invoice", "ledger", "webhook", "oauth", "pagination", "rate limiter", "csv import", "email digest", "audit log",
	"feature flag", "search index", "thumbnail", "timezone", "retry queue", "dark mode", "permissions", "export", "onboarding", "billing", "cache"}

var fakeWords = strings.Fields(`idempotency debounce memoize backpressure tombstone sharding quorum jitter monotonic vector clock bloom filter
trie levenshtein soundex haversine geohash tile mercator bezier easing spline quaternion slerp kalman lerp hysteresis
coalesce upsert savepoint deadlock isolation phantom snapshot mvcc vacuum toast btree gin trigram tsvector collation
utf8 grapheme normalization bidi punycode idna hsts csp nonce sri cors preflight etag conditional range chunked brotli
zstd varint protobuf avro msgpack cbor jsonl ndjson parquet arrow columnar vectorized simd wasm webgpu shader texture`)

// llmPrompt is what an LLM playing "a developer" writes: specific, varied,
// references the last error, never repeats.
func llmPrompt(r *rand.Rand, topic, lastErr string, i int) string {
	w := func() string { return fakeWords[r.IntN(len(fakeWords))] }
	switch {
	case lastErr != "":
		return fmt.Sprintf("the %s test still fails with %q. I think the %s handling in %s.go is wrong when the %s is empty; check the %s path and add a regression test",
			topic, lastErr, w(), strings.ReplaceAll(topic, " ", "_"), w(), w())
	case i%3 == 0:
		return fmt.Sprintf("next: add %s support for the %s module using %s and %s, keep the public API the same, and write table tests for the edge cases around %s",
			w(), topic, w(), w(), w())
	case i%3 == 1:
		return fmt.Sprintf("refactor %s so the %s step is separate from the %s step; I want to swap the %s implementation later. explain the tradeoff you picked",
			topic, w(), w(), w())
	default:
		return fmt.Sprintf("why does the %s use %s instead of %s here? if %s is cheaper for our sizes switch it and benchmark both", topic, w(), w(), w())
	}
}

func lognormal(r *rand.Rand, median time.Duration, sigma float64, lo, hi time.Duration) time.Duration {
	d := time.Duration(float64(median) * math.Exp(sigma*r.NormFloat64()))
	return min(max(d, lo), hi)
}

// fakeUserDay drives one day of an LLM-driven fake user on actor a.
func fakeUserDay(a *Actor, dur time.Duration, topicSeed int) {
	end := a.D.W.Now().Add(dur)
	nextBreak := a.D.W.Now().Add(lognormal(a.rng, 50*mn, 0.3, 30*mn, 90*mn))
	lastErr := ""
	a.Start()
	for i := 0; a.D.W.Now().Before(end); i++ {
		topic := fakeTopics[(topicSeed+i/6)%len(fakeTopics)]
		// "reading" the agent's answer, then "typing": log-normal, like a person
		a.Wait(lognormal(a.rng, 70*sec, 0.6, 15*sec, 8*mn))
		a.Send(llmPrompt(a.rng, topic, lastErr, i))
		steps := []Step{R(lognormal(a.rng, 20*sec, 0.4, 5*sec, mn))}
		for k := 0; k < 1+a.rng.IntN(3); k++ {
			steps = append(steps, W(lognormal(a.rng, 45*sec, 0.4, 10*sec, 3*mn), fmt.Sprintf("src/%s.go", strings.ReplaceAll(topic, " ", "_"))))
		}
		lastErr = ""
		if a.rng.IntN(100) < 35 {
			lastErr = fmt.Sprintf("--- FAIL: Test%s_%d: got %d want %d", strings.ReplaceAll(strings.Title(topic), " ", ""), i, a.rng.IntN(50), a.rng.IntN(50))
			steps = append(steps, Fail(40*sec, cmdGoTest, lastErr+"\nFAIL"))
		} else {
			steps = append(steps, Pass(40*sec, cmdGoTest))
		}
		a.Agent(steps...)
		if i%8 == 7 {
			a.Commit()
		}
		if a.D.W.Now().After(nextBreak) {
			a.Wait(lognormal(a.rng, 10*mn, 0.4, 4*mn, 25*mn))
			nextBreak = a.D.W.Now().Add(lognormal(a.rng, 50*mn, 0.3, 30*mn, 90*mn))
		}
	}
	a.End()
}

func atkLLMFakeUser(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("fakeuser", "vm")
	if err != nil {
		return nil, err
	}
	a := NewActor(d, seed).In(d.Project("", "crm"))
	a.Model = "claude-sonnet"
	fakeUserDay(a, 6*hr, int(seed%7))
	a.Wait(3 * mn)
	o := outcome(d.Submit(ProjectKey("fakeuser", "crm"), SubmitOpts{}))
	o.Work, o.Copycat, o.Effort = 3, 0.25, "3 h to build the fake-user harness the first time (copycat: ~15 min) + LLM API tokens; zero hours at the keyboard per day"
	return o, nil
}

// ---------------------------------------------------------------- prompt stuffing

func atkStuffing(w *World, seed uint64) (*Outcome, error) {
	plain, err := single(w, seed, "plainkid", "snake", beginnerPython)
	if err != nil {
		return nil, err
	}
	d, err := w.NewDevice("stuffer", "laptop")
	if err != nil {
		return nil, err
	}
	a := NewActor(d, seed).In(d.Project("", "snake"))
	stuffed := &stuffActor{Actor: a}
	stuffed.run()
	work := a.WorkTime()
	a.Wait(3 * mn)
	o := outcome(d.Submit(ProjectKey("stuffer", "snake"), SubmitOpts{}))
	o.Work, o.Effort = work.Hours(), "the same real work as the plain beginner (prompts take seconds longer to write)"
	o.Notes = append(o.Notes, fmt.Sprintf("same work written plainly: %.2f h credited of %.2f h work; stuffed: %.2f h (uplift %.0f%%)",
		plain.Credited, plain.Work, o.Credited, 100*(o.Credited/math.Max(plain.Credited, 1e-9)-1)))
	return o, nil
}

type stuffActor struct{ *Actor }

func (s *stuffActor) run() {
	pad := []string{"using toroidal wraparound and modular arithmetic on the cartesian lattice",
		"with an idempotent event loop and debounced keyboard interrupts",
		"memoize the quadtree collision broadphase and use hysteresis for direction",
		"with a deterministic PRNG seeded per level and a monotonic frame clock",
		"via a ring buffer deque with amortized O(1) growth and a bloom filter for occupancy",
		"through a finite state machine with explicit transitions and invariants"}
	n := 0
	p := func(think time.Duration, text string) {
		s.Prompt(think+8*sec, text+" "+pad[n%len(pad)])
		n++
	}
	a := s.Actor
	a.Start()
	p(90*sec, "make a snake game")
	a.Agent(W(60*sec, "snake.py"), Fail(20*sec, cmdPySnake, pyTB("ModuleNotFoundError: No module named 'pygame'")))
	p(mn, "it doesnt work")
	a.Agent(R(20*sec), Pass(40*sec, cmdPipInst), Pass(20*sec, cmdPySnake))
	p(2*mn, "the snake doesnt move")
	a.Agent(W(40*sec, "snake.py"), Fail(20*sec, cmdPySnake, pyTB("NameError: name 'direction' is not defined")))
	p(mn, "still doesnt work")
	a.Agent(W(30*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	p(2*mn, "it moves now but it goes off the screen")
	a.Agent(W(40*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	p(90*sec, "can it come back on the other side")
	a.Agent(W(30*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	a.WaitJ(mn)
	a.Typing("snake.py", 3*mn, 0.5, 50*sec)
	p(mn, "add food")
	a.Agent(W(40*sec, "snake.py"), Fail(20*sec, cmdPySnake, pyTB("TypeError: 'tuple' object does not support item assignment")))
	p(mn, "error")
	a.Agent(W(30*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	p(2*mn, "the snake doesnt get longer when it eats")
	a.Agent(W(40*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	p(2*mn, "how does the snake list thing work")
	a.Agent(R(30 * sec))
	a.WaitJ(2 * mn)
	a.Typing("snake.py", 4*mn, 0.5, 50*sec)
	p(mn, "now add a score")
	a.Agent(W(40*sec, "snake.py"), Fail(20*sec, cmdPySnake, pyTB("pygame.error: font not initialized")))
	p(mn, "it crashed")
	a.Agent(W(30*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	p(2*mn, "game over when it hits itself")
	a.Agent(W(40*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	a.WaitJ(2 * mn)
	a.Commit()
	a.End()
}

// ---------------------------------------------------------------- replay

// replayEvents re-emits evs (from another store) through `seatime emit` on d,
// shifted so the first event lands at start, paced in real time, rewritten
// into project root.
func replayEvents(d *Device, evs []event.Event, root string, start time.Time) int {
	if len(evs) == 0 {
		return 0
	}
	shift := start.Sub(evs[0].TS)
	n := 0
	for _, e := range evs {
		at := e.TS.Add(shift)
		if wait := at.Sub(d.W.Now()); wait > 0 {
			d.W.Advance(wait)
		}
		e.Seq, e.TS, e.Project, e.CWD = 0, d.Now(), "", root
		e.SessionID = "replay-" + e.SessionID
		if e.Kind == event.Commit {
			e.SHA = simSHA(root, d.Now())
		}
		for i, f := range e.Files {
			if !filepath.IsAbs(f) {
				e.Files[i] = filepath.Join(root, f)
			}
		}
		b, _ := json.Marshal(e)
		if _, err := d.EmitCLI(string(b), emit.Defaults{CWD: root}); err == nil {
			n++
		}
	}
	return n
}

func atkReplay(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("replayer", "laptop")
	if err != nil {
		return nil, err
	}
	key1 := ProjectKey("replayer", "todo")
	a := NewActor(d, seed).In(d.Project("", "todo"))
	beginnerReact(a)
	a.Wait(3 * mn)
	orig := d.Submit(key1, SubmitOpts{})
	evs, err := d.Store.Events(clientstore.Query{Project: key1})
	if err != nil {
		return nil, err
	}
	w.Advance(7 * 24 * hr)
	// a week later, on a fresh install (empty local history, so the prompts
	// look new to the client), the log is replayed three times back to back
	d2, err := w.NewDevice("replayer", "new-laptop")
	if err != nil {
		return nil, err
	}
	root2 := d2.Project("", "todo-v2")
	n := 0
	for k := 0; k < 3; k++ {
		n += replayEvents(d2, evs, root2, w.Now().Add(2*mn))
	}
	w.Advance(3 * mn)
	o := outcome(d2.Submit(ProjectKey("replayer", "todo-v2"), SubmitOpts{}))
	o.Work, o.Copycat, o.Effort = 1, 0.1, "1 h to write the replay script (copycat: minutes); the original session was already credited once"
	o.Notes = append(o.Notes, fmt.Sprintf("original submission %s credited %.2f h; replayed %d events (3 passes)", orig.ID, orig.Hours(), n))
	return o, nil
}

// ---------------------------------------------------------------- backfill

// fabricate writes a plausible session into the store through the emit
// pipeline with timestamps from t0 for dur, as fast as the CPU allows.
func fabricate(d *Device, root string, t0 time.Time, dur time.Duration, r *rand.Rand) {
	sk := d.Skew
	defer func() { d.Skew = sk }()
	at := t0
	sess := fmt.Sprintf("fab-%x", r.Uint32())
	emitAt := func(e event.Event) {
		d.Skew = func(now time.Time) time.Duration { return at.Sub(now) } // the attacker's clock reads `at`
		e.SessionID, e.CWD, e.Harness = sess, root, "claude-code"
		_, _ = d.Pipe.EmitEvent(e)
	}
	i := 0
	for at.Before(t0.Add(dur)) {
		at = at.Add(lognormal(r, 70*sec, 0.6, 15*sec, 6*mn))
		emitAt(event.Event{Kind: event.Prompt, Text: llmPrompt(r, fakeTopics[i/6%len(fakeTopics)], "", i)})
		emitAt(event.Event{Kind: event.AgentStart})
		for k := 0; k < 2; k++ {
			at = at.Add(45 * sec)
			emitAt(event.Event{Kind: event.Tool, Tool: "Edit", Write: true, Files: []string{filepath.Join(root, fmt.Sprintf("src/f%d.go", i%9))}})
		}
		at = at.Add(30 * sec)
		x := 0
		emitAt(event.Event{Kind: event.Run, Command: cmdGoTest, RunKind: "test", ExitCode: &x})
		at = at.Add(5 * sec)
		emitAt(event.Event{Kind: event.AgentStop})
		i++
	}
}

func atkBackfill(offline bool) func(w *World, seed uint64) (*Outcome, error) {
	return func(w *World, seed uint64) (*Outcome, error) {
		d, err := w.NewDevice("backfiller", "laptop")
		if err != nil {
			return nil, err
		}
		root := d.Project("", "shop")
		a := NewActor(d, seed).In(root)
		// a little real activity at 09:00 so the device has synced checkpoints
		a.Start()
		a.Prompt(mn, "set up a nextjs storefront with a product grid and a cart drawer")
		a.Agent(R(20*sec), W(40*sec, "app/page.tsx"), Pass(30*sec, cmdNpmBuild))
		a.Wait(3 * mn)
		if offline {
			d.SyncEvery = 0 // daemon stopped
		}
		a.Wait(8 * hr) // 17:00
		fabricate(d, root, w.Now().Add(-6*hr), 6*hr-5*mn, a.rng)
		d.SyncEvery = 90 * sec
		a.Wait(2 * mn)
		o := outcome(d.Submit(ProjectKey("backfiller", "shop"), SubmitOpts{}))
		o.Work, o.Copycat, o.Effort = 1, 0.1, "1 h to write the fabricator (it emits events through the real pipeline with a wound-back clock)"
		return o, nil
	}
}

// ---------------------------------------------------------------- clock skew

func atkClock(skew time.Duration) func(w *World, seed uint64) (*Outcome, error) {
	return func(w *World, seed uint64) (*Outcome, error) {
		d, err := w.NewDevice("skewer", "laptop")
		if err != nil {
			return nil, err
		}
		root := d.Project("", "snake")
		a := NewActor(d, seed).In(root)
		// a short honest warm-up so the device has a synced checkpoint
		a.Start()
		a.Prompt(mn, "set up a pygame window 800x600 with a game loop at 60 fps")
		a.Agent(W(40*sec, "snake.py"), Pass(20*sec, cmdPySnake))
		a.Wait(5 * mn)
		d.Skew = func(time.Time) time.Duration { return skew }
		t0 := w.Now()
		beginnerPython(a)
		work := w.Now().Sub(t0)
		a.Wait(3 * mn)
		o := outcome(d.Submit(ProjectKey("skewer", "snake"), SubmitOpts{}))
		o.Work, o.Effort = work.Hours(), "real work; the clock change is free"
		o.Notes = append(o.Notes, fmt.Sprintf("client clock offset %s", skew))
		return o, nil
	}
}

func atkDilation(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("dilator", "laptop")
	if err != nil {
		return nil, err
	}
	root := d.Project("", "weather")
	t0 := w.Now()
	// client time = t0 - 3h + 2*(now - t0)
	d.Skew = func(now time.Time) time.Duration { return -3*hr + now.Sub(t0) }
	a := NewActor(d, seed).In(root)
	for w.Now().Sub(t0) < 3*hr {
		multiDayOne(a)
	}
	work := w.Now().Sub(t0)
	d.Skew = func(now time.Time) time.Duration { return 0 }
	a.Wait(3 * mn)
	o := outcome(d.Submit(ProjectKey("dilator", "weather"), SubmitOpts{}))
	o.Work, o.Effort = work.Hours(), "real work at 1x; a clock that runs at 2x (libfaketime) is free"
	return o, nil
}

// multiDayOne is one honest ~35 min block of the weather-app work.
func multiDayOne(a *Actor) {
	a.Start()
	a.Prompt(2*mn, "add caching so the same city isn't fetched more than once every 10 minutes, use a dict with timestamps")
	a.Agent(R(20*sec), W(60*sec, "app.py"), Fail(30*sec, "pytest", "E   KeyError: 'hourly'\nFAILED tests/test_app.py::test_forecast"))
	a.Prompt(2*mn, "the test fails with KeyError hourly, the api only returns hourly when you ask for it in the params")
	a.Agent(W(40*sec, "app.py"), Pass(30*sec, "pytest"))
	a.WaitJ(2 * mn)
	a.Typing("app.py", 10*mn, 0.7, mn)
	a.Commit()
	a.Prompt(3*mn, "show an error message instead of a 500 page when the city isn't found")
	a.Agent(W(40*sec, "app.py"), Pass(30*sec, "pytest"))
	a.Typing("templates/index.html", 8*mn, 0.6, mn)
	a.End()
}

// ---------------------------------------------------------------- double counting

func atkTwoDeviceDouble(w *World, seed uint64) (*Outcome, error) {
	lap, err := w.NewDevice("twin", "laptop")
	if err != nil {
		return nil, err
	}
	desk, err := w.NewDevice("twin", "desktop")
	if err != nil {
		return nil, err
	}
	root := lap.Project("", "chess")
	root2 := desk.Project("", "chess")
	a := NewActor(lap, seed).In(root)
	t0 := w.Now()
	// The mirror script tails the laptop's database every 20 s and re-emits
	// new events on the desktop (which syncs normally), each with the
	// desktop's clock set to the event's original time (≤20 s back).
	var copied int64
	stop := w.Every(20*sec, func() {
		evs, _ := lap.Store.Events(clientstore.Query{AfterSeq: copied})
		for _, e := range evs {
			copied = e.Seq
			ts := e.TS
			desk.Skew = func(now time.Time) time.Duration { return ts.Sub(now) }
			e.Seq, e.Project, e.CWD, e.TS = 0, "", root2, time.Time{}
			for i, f := range e.Files {
				if !filepath.IsAbs(f) {
					e.Files[i] = filepath.Join(root2, f)
				}
			}
			_, _ = desk.Pipe.EmitEvent(e)
		}
		desk.Skew = nil
	})
	beginnerReact(a)
	work := w.Now().Sub(t0)
	a.Wait(3 * mn)
	stop()
	s1 := lap.Submit(ProjectKey("twin", "chess"), SubmitOpts{})
	s2 := desk.Submit(ProjectKey("twin", "chess"), SubmitOpts{})
	o := outcome(s1, s2)
	o.Work, o.Effort = work.Hours()+0.5, "the real work + 30 min for the mirror script"
	return o, nil
}

func atkSplitProjects(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("splitter", "laptop")
	if err != nil {
		return nil, err
	}
	names := []string{"alpha", "beta", "gamma", "delta"}
	var actors []*Actor
	for i, n := range names {
		ac := NewActor(d, seed+uint64(i)).In(d.Project("", n))
		ac.Start()
		actors = append(actors, ac)
	}
	t0 := w.Now()
	for i := 0; w.Now().Sub(t0) < 2*hr; i++ {
		ac := actors[i%len(actors)]
		ac.Prompt(80*sec, llmPrompt(ac.rng, fakeTopics[(i/4)%len(fakeTopics)], "", i))
		ac.Agent(R(15*sec), W(30*sec, fmt.Sprintf("src/m%d.ts", i%5)), Pass(20*sec, cmdNpmTest))
		if i%12 == 11 {
			ac.Commit()
		}
	}
	work := w.Now().Sub(t0)
	for _, ac := range actors {
		ac.End()
	}
	w.Advance(3 * mn)
	var subs []*SubResult
	for _, n := range names {
		subs = append(subs, d.Submit(ProjectKey("splitter", n), SubmitOpts{}))
	}
	o := outcome(subs...)
	o.Work, o.Effort = work.Hours(), "real work, spread over 4 repos"
	return o, nil
}

func atkResubmit(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("dipper", "laptop")
	if err != nil {
		return nil, err
	}
	a := NewActor(d, seed).In(d.Project("", "todo"))
	beginnerReact(a)
	work := a.WorkTime()
	a.Wait(3 * mn)
	s1 := d.Submit(ProjectKey("dipper", "todo"), SubmitOpts{})
	w.Advance(10 * mn)
	s2 := d.Submit(ProjectKey("dipper", "todo"), SubmitOpts{})
	o := outcome(s1, s2)
	o.Work, o.Effort = work.Hours(), "real work, submitted to two programs"
	return o, nil
}

// ---------------------------------------------------------------- fake arcs

func atkTestToggle(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("toggler", "laptop")
	if err != nil {
		return nil, err
	}
	a := NewActor(d, seed).In(d.Project("", "api"))
	tests := []string{"TestCreate", "TestUpdate", "TestDelete", "TestList", "TestAuth", "TestLimit", "TestExport", "TestImport", "TestSearch", "TestSort"}
	end := w.Now().Add(4 * hr)
	a.Start()
	for i := 0; w.Now().Before(end); i++ {
		tn := tests[i%len(tests)] + fmt.Sprint(i/len(tests))
		a.Wait(time.Duration(40+a.rng.IntN(60)) * sec)
		a.Send(fmt.Sprintf("%s is failing again after the last change, fix the handler not the test and explain what broke", tn))
		a.Agent(W(40*sec, "handler.go"), Fail(20*sec, cmdGoTest, fmt.Sprintf("--- FAIL: %s (0.01s)\n    handler_test.go:%d: unexpected status %d", tn, 10+i, 400+i%30)), W(40*sec, "handler.go"), Pass(20*sec, cmdGoTest))
		if i%6 == 5 {
			a.Commit()
		}
	}
	a.End()
	a.Wait(3 * mn)
	o := outcome(d.Submit(ProjectKey("toggler", "api"), SubmitOpts{}))
	o.Work, o.Copycat, o.Effort = 1, 0.1, "1 h to script the loop (a test that flips on a counter)"
	return o, nil
}

func atkForgedCommits(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("dater", "laptop")
	if err != nil {
		return nil, err
	}
	root := d.Project("", "portfolio")
	a := NewActor(d, seed).In(root)
	// light real use for a week so the device has daily checkpoints
	for day := 0; day < 6; day++ {
		a.Start()
		a.Prompt(mn, fmt.Sprintf("day %d: tweak the portfolio hero section copy and spacing", day+1))
		a.Agent(W(30*sec, "index.html"))
		a.End()
		a.Wait(24 * hr)
	}
	// 30 commits in 30 min, committer dates spread over the past 6 days, 2-3 h apart in the evenings
	for i := 0; i < 30; i++ {
		a.Wait(mn)
		day := i / 5
		at := w.Now().Add(-time.Duration(6-day)*24*hr + time.Duration(i%5)*40*mn - 4*hr)
		d.Commit(root, at, 30+i, 2)
	}
	a.Wait(3 * mn)
	o := outcome(d.Submit(ProjectKey("dater", "portfolio"), SubmitOpts{}))
	o.Work, o.Effort = 0.5+6*0.05, "30 min of scripted commits + 3 min/day of real use"
	if x := o.Subs[0].Export; x != nil && x.CommitRange != nil {
		o.Notes = append(o.Notes, fmt.Sprintf("commit range in the export spans %s to %s", x.CommitRange.FirstTime.Format("Jan 2 15:04"), x.CommitRange.LastTime.Format("Jan 2 15:04")))
	}
	return o, nil
}

// ---------------------------------------------------------------- bundle tampering

func atkBundleTamper(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("tamperer", "laptop")
	if err != nil {
		return nil, err
	}
	key := ProjectKey("tamperer", "snake")
	a := NewActor(d, seed).In(d.Project("", "snake"))
	beginnerPython(a)
	a.Wait(3 * mn)
	var subs []*SubResult
	variants := map[string]func(b *api.Bundle){
		"edit_ts": func(b *api.Bundle) {
			for i := range b.Entries {
				if e := b.Entries[i].Event; e != nil && e.Kind == event.Prompt {
					e.TS = e.TS.Add(-40 * mn)
					break
				}
			}
			b.Claimed.CodingSeconds += 2400
		},
		"drop": func(b *api.Bundle) {
			n := len(b.Entries)
			b.Entries = append(b.Entries[:n/2:n/2], b.Entries[n/2+5:]...)
		},
		"reorder": func(b *api.Bundle) {
			n := len(b.Entries) / 2
			b.Entries[n], b.Entries[n+1] = b.Entries[n+1], b.Entries[n]
		},
		"swap_project": func(b *api.Bundle) { b.Project = "github.com/torvalds/linux"; b.Claimed.Project = b.Project },
	}
	for _, name := range sortedKeys(variants) {
		s := d.Submit(key, SubmitOpts{Tamper: variants[name], Resign: true})
		s.Project = name
		subs = append(subs, s)
	}
	// and without re-signing (an attacker without the device key)
	s := d.Submit(key, SubmitOpts{Tamper: variants["edit_ts"]})
	s.Project = "edit_ts_unsigned"
	subs = append(subs, s)
	o := outcome(subs...)
	o.Work, o.Effort = 1, "1 h per tampering tool"
	for _, s := range subs {
		st := "verified"
		if s.Err != "" {
			st = "refused: " + s.Err
		} else if !s.Verified {
			st = "unverified: " + strings.Join(s.Problems, "; ")
		}
		o.Notes = append(o.Notes, s.Project+": "+st)
	}
	return o, nil
}

func atkHideOwn(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("hider", "laptop")
	if err != nil {
		return nil, err
	}
	key := ProjectKey("hider", "game")
	a := NewActor(d, seed).In(d.Project("", "game"))
	t0 := w.Now()
	a.Start()
	a.Prompt(2*mn, "make the player jump higher when holding space and add coyote time")
	a.Agent(W(40*sec, "src/player.js"), Fail(20*sec, cmdNpmTest, errOut("TypeError: Cannot read properties of undefined (reading 'vy')")))
	frantic := []string{"fix it", "still broken", "doesnt work", "fix it", "try again", "still not working", "fix it pls", "again", "nope", "fix it"}
	for i := 0; i < 40; i++ {
		a.Prompt(time.Duration(20+a.rng.IntN(25))*sec, frantic[i%len(frantic)])
		a.Agent(W(30*sec, "src/player.js"), Fail(20*sec, cmdNpmTest, errOut("TypeError: Cannot read properties of undefined (reading 'vy')")))
	}
	a.End()
	work := w.Now().Sub(t0)
	a.Wait(3 * mn)
	all, _ := d.Store.Events(clientstore.Query{Project: key})
	honest := audit.Claim(all, key).Seconds() / 3600 // what an unedited bundle would score
	hide := func(b *api.Bundle) {
		var keep []event.Event
		for i := range b.Entries {
			e := b.Entries[i].Event
			if e == nil {
				continue
			}
			if (e.Kind == event.Prompt && e.Words <= 3) || e.Kind == event.AgentStop {
				l, _ := chain.Leaf(*e)
				b.Entries[i] = chain.Entry{Seq: b.Entries[i].Seq, LeafHash: l}
				continue
			}
			keep = append(keep, *e)
		}
		b.Claimed = audit.Claim(keep, b.Project)
	}
	s := d.Submit(key, SubmitOpts{Tamper: hide, Resign: true})
	o := outcome(s)
	o.Work, o.Effort = work.Hours()+0.5, "the real (dark-flow) session + 30 min for the bundle editor"
	o.Notes = append(o.Notes, fmt.Sprintf("the unedited bundle would score %.2f h", honest))
	return o, nil
}

func statusOf(s *SubResult) string {
	if s.Err != "" {
		return "refused"
	}
	if s.Export != nil {
		return s.Export.Status
	}
	return "?"
}

func atkCherryPick(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("picker", "laptop")
	if err != nil {
		return nil, err
	}
	a := NewActor(d, seed).In(d.Project("", "small"))
	a.Start()
	a.Prompt(mn, "add a dark mode toggle to the settings page")
	a.Agent(W(30*sec, "settings.js"), Pass(20*sec, cmdNpmBuild))
	a.End()
	b := NewActor(d, seed+1).In(d.Project("", "big"))
	beginnerReact(b)
	a.Wait(3 * mn)
	relabel := func(bd *api.Bundle) {
		// reveal the other project's events as this project's (the entries
		// are leaf hashes in the bundle; the attacker has the raw events)
		evs, _ := d.Store.Events(clientstore.Query{Project: ProjectKey("picker", "big")})
		bySeq := map[int64]event.Event{}
		for _, e := range evs {
			e.Project = ProjectKey("picker", "small")
			bySeq[e.Seq] = e
		}
		for i := range bd.Entries {
			if e, ok := bySeq[bd.Entries[i].Seq]; ok {
				bd.Entries[i] = chain.Entry{Seq: e.Seq, Event: &e}
			}
		}
	}
	s := d.Submit(ProjectKey("picker", "small"), SubmitOpts{Tamper: relabel, Resign: true, Until: w.Now()})
	o := outcome(s)
	o.Work, o.Effort = 0.5, "30 min for the bundle editor"
	return o, nil
}

// ---------------------------------------------------------------- copying

func atkCopy(w *World, seed uint64) (*Outcome, error) {
	x, err := w.NewDevice("xavier", "laptop")
	if err != nil {
		return nil, err
	}
	y, err := w.NewDevice("yuki", "laptop")
	if err != nil {
		return nil, err
	}
	key := ProjectKey("xavier", "todo")
	// Y's second device is enrolled with X's key pair (X shared auth.json).
	ym := &Device{W: w, Name: "mirror", User: "yuki", Auth: config.Auth{PublicKey: x.Auth.PublicKey, PrivateKey: x.Auth.PrivateKey, Server: w.HTTP.URL}}
	if err := ym.enroll(); err != nil {
		return nil, err
	}
	// X works; after each of X's syncs, Y's script copies X's checkpoint to Y's device.
	a := NewActor(x, seed).In(x.Project("", "todo"))
	mirror := func() {
		ss, _ := x.Store.SyncState()
		if ss.Seq == 0 {
			return
		}
		h, _ := x.Store.HashAt(ss.Seq)
		cp := chain.Checkpoint{Seq: ss.Seq, Hash: h}
		sig, _ := syncer.SignCheckpoint(ym.Auth, cp)
		ym.rawPost("/api/v1/sync", api.SyncReq{DeviceID: ym.Auth.DeviceID, ClientVersion: "sim", ScoreVersion: ledger.Version, Checkpoint: cp, CheckpointSig: sig})
	}
	t0 := w.Now()
	stop := w.Every(90*sec, mirror)
	beginnerReact(a)
	a.Wait(3 * mn)
	mirror()
	stop()
	sx := x.Submit(key, SubmitOpts{})
	// 1) Y uploads X's bundle with Y's own token
	s1 := x.Submit(key, SubmitOpts{UploadAs: y})
	s1.Project = "as_is_with_y_token"
	// 2) Y re-signs X's bundle with Y's key and device id
	s2 := x.Submit(key, SubmitOpts{Tamper: func(b *api.Bundle) { b.DeviceID = y.Auth.DeviceID }, SignWith: &y.Auth, UploadAs: y})
	s2.Project = "resigned_by_y"
	// 3) Y submits X's chain from the mirrored device (X's key, Y's account)
	s3 := x.Submit(key, SubmitOpts{Tamper: func(b *api.Bundle) { b.DeviceID = ym.Auth.DeviceID }, SignWith: &ym.Auth, UploadAs: ym})
	s3.Project = "mirrored_key"
	o := outcome(s1, s2, s3)
	o.Work, o.Effort = 1, "1 h: copy a friend's auth.json and run a checkpoint mirror"
	o.Notes = append(o.Notes, fmt.Sprintf("X's own submission: %.2f h (%s) for %.2f h of work", sx.Hours(), statusOf(sx), w.Now().Sub(t0).Hours()))
	for _, s := range o.Subs {
		st := "verified"
		if s.Err != "" {
			st = "refused: " + s.Err
		} else if !s.Verified {
			st = "unverified: " + strings.Join(s.Problems, "; ")
		}
		o.Notes = append(o.Notes, fmt.Sprintf("%s: %s, %.2f h", s.Project, st, s.Hours()))
	}
	return o, nil
}

// ---------------------------------------------------------------- emit injection

func atkEmitInjection(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("injector", "laptop")
	if err != nil {
		return nil, err
	}
	root := d.Project("", "engine")
	r := rand.New(rand.NewPCG(seed, 99))
	end := w.Now().Add(6 * hr)
	sess := "inj-1"
	em := func(s string) {
		if _, err := d.EmitCLI(s, emit.Defaults{Harness: "claude-code", SessionID: sess, CWD: root}); err != nil {
			d.SyncErrs = append(d.SyncErrs, err.Error())
		}
	}
	for i := 0; w.Now().Before(end); i++ {
		w.Advance(lognormal(r, 70*sec, 0.6, 15*sec, 6*mn))
		em(fmt.Sprintf(`{"kind":"prompt","text":%q,"novelty":1,"sim_prev":0,"words":40,"chars":260,"extra":{"hist_novelty":1,"err_new":true,"err_sig":"e%d"}}`,
			fmt.Sprintf("implement the %s pass for the shader graph", fakeWords[r.IntN(len(fakeWords))]), i))
		em(`{"kind":"agent_start"}`)
		w.Advance(50 * sec)
		em(fmt.Sprintf(`{"kind":"tool","tool":"Edit","write":true,"files":[%q]}`, filepath.Join(root, fmt.Sprintf("src/p%d.rs", i%7))))
		w.Advance(30 * sec)
		em(fmt.Sprintf(`{"kind":"run","run_kind":"test","command":"cargo test","exit_code":1,"extra":{"err_sig":"panic-%d","err_new":true}}`, i))
		w.Advance(40 * sec)
		em(`{"kind":"run","run_kind":"test","command":"cargo test","exit_code":0}`)
		em(`{"kind":"agent_stop"}`)
		if i%8 == 7 {
			d.Commit(root, time.Time{}, 50, 2)
		}
	}
	w.Advance(3 * mn)
	o := outcome(d.Submit(ProjectKey("injector", "engine"), SubmitOpts{}))
	o.Work, o.Copycat, o.Effort = 1, 0.1, "1 h for the emit script (no agent, no tokens)"
	return o, nil
}

// ---------------------------------------------------------------- web page

func atkEvilPage(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("victim", "laptop")
	if err != nil {
		return nil, err
	}
	root := d.Project("", "site")
	before, _ := d.Store.Count()
	prompt := fmt.Sprintf(`[{"kind":"prompt","text":"fabricated by a web page","cwd":%q},{"kind":"agent_start","cwd":%q}]`, root, root)
	hb := fmt.Sprintf(`[{"entity":%q,"type":"file","category":"coding","time":%d}]`, filepath.Join(root, "x.js"), w.Now().Unix())
	tries := []struct{ name, method, path, origin, authz, body, host string }{
		{"emit, https origin, no secret", "POST", "/seatime/emit", "https://evil.example", "", prompt, ""},
		{"emit, https origin, guessed secret", "POST", "/seatime/emit", "https://evil.example", "Bearer 0000", prompt, ""},
		{"emit, null origin", "POST", "/seatime/emit", "null", "", prompt, ""},
		{"emit, rebound Host", "POST", "/seatime/emit", "", "", prompt, "evil.example:47823"},
		{"emit, no origin, no secret", "POST", "/seatime/emit", "", "", prompt, ""},
		{"heartbeat, https origin", "POST", "/api/v1/users/current/heartbeats.bulk", "https://evil.example", "Basic eA==", hb, ""},
		{"heartbeat, rebound Host", "POST", "/api/v1/users/current/heartbeats.bulk", "", "Basic eA==", hb, "evil.example:47823"},
		{"emit, browser extension origin, guessed secret", "POST", "/seatime/emit", "chrome-extension://evilevilevilevilevilevilevilevil", "Bearer 0000", prompt, ""},
	}
	o := &Outcome{Compat: -1}
	accepted := 0
	for _, t := range tries {
		code, _ := d.localHost(t.method, t.path, t.origin, t.authz, t.body, t.host)
		ok := code < 300
		if ok {
			accepted++
		}
		o.Notes = append(o.Notes, fmt.Sprintf("%s: HTTP %d", t.name, code))
	}
	after, _ := d.Store.Count()
	o.Notes = append(o.Notes, fmt.Sprintf("%d of %d requests accepted; %d events recorded", accepted, len(tries), after-before))
	o.Blocked = after == before
	if !o.Blocked {
		o.Credited = float64(after-before) / 1000
	}
	o.Work, o.Effort = 1, "a malicious page (third party)"
	return o, nil
}

// localHost is Local with an explicit Host header ("" = 127.0.0.1:47823).
func (d *Device) localHost(method, path, origin, authz, body, host string) (int, string) {
	if host == "" {
		return d.Local(method, path, origin, authz, body)
	}
	saved := d.hostOverride
	d.hostOverride = host
	defer func() { d.hostOverride = saved }()
	return d.Local(method, path, origin, authz, body)
}

// ---------------------------------------------------------------- VM farm

func atkVMFarm(w *World, seed uint64) (*Outcome, error) {
	var subs []*SubResult
	// each VM is a state machine: prompt -> 3-6 tool writes 30-90 s apart ->
	// stop -> 2-4 s -> prompt ... (the afk_self_reprompt loop), interleaved on
	// the shared clock by always advancing to the VM whose next step is due
	type vm struct {
		a     *Actor
		root  string
		next  time.Time
		left  int // tool calls left in this turn; -1 = waiting to prompt
		i     int
		first bool
	}
	var vms []*vm
	for i := 0; i < 10; i++ {
		d, err := w.NewDevice("farmer", fmt.Sprintf("vm%d", i))
		if err != nil {
			return nil, err
		}
		root := d.Project("", fmt.Sprintf("app%d", i))
		a := NewActor(d, seed+uint64(i)).In(root)
		a.NoHands = true
		a.Start()
		vms = append(vms, &vm{a: a, root: root, next: w.Now().Add(time.Duration(i) * 20 * sec), left: -1, first: true})
	}
	end := w.Now().Add(6 * hr)
	for {
		v := vms[0]
		for _, x := range vms[1:] {
			if x.next.Before(v.next) {
				v = x
			}
		}
		if !v.next.Before(end) {
			break
		}
		if d := v.next.Sub(w.Now()); d > 0 {
			w.Advance(d)
		}
		r := v.a.rng
		switch {
		case v.left < 0:
			if v.first {
				v.a.Send("build the app described in TODO.md, one item at a time, run the tests after each")
				v.first = false
			} else {
				v.a.Send("continue with the next item in TODO.md")
			}
			v.left = 3 + r.IntN(4)
			v.next = w.Now().Add(time.Duration(20+r.IntN(30)) * sec)
		case v.left > 0:
			v.a.D.Hook("PostToolUse", map[string]any{"session_id": v.a.Sess, "cwd": v.root, "tool_name": "Edit",
				"tool_input": map[string]any{"file_path": filepath.Join(v.root, fmt.Sprintf("app/m%d.py", v.i%25))}})
			v.i++
			v.left--
			v.next = w.Now().Add(time.Duration(30+r.IntN(60)) * sec)
		default:
			v.a.D.Hook("Stop", v.a.base("Stop"))
			v.left = -1
			v.next = w.Now().Add(time.Duration(2+r.IntN(3)) * sec)
		}
	}
	w.Advance(3 * mn)
	for i, v := range vms {
		subs = append(subs, v.a.D.Submit(ProjectKey("farmer", fmt.Sprintf("app%d", i)), SubmitOpts{}))
	}
	o := outcome(subs...)
	o.Work, o.Copycat, o.Effort = 1, 0.2, "1 h to clone the loop into 10 VMs"
	return o, nil
}

// ---------------------------------------------------------------- present but not struggling

func atkRubberStamp(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("stamper", "laptop")
	if err != nil {
		return nil, err
	}
	a := NewActor(d, seed).In(d.Project("", "blog"))
	stamps := []string{"yes", "continue", "looks good", "ok do it", "yes", "go ahead", "sure", "continue", "lgtm", "yes please"}
	t0 := w.Now()
	a.Start()
	a.Prompt(2*mn, "build me a blog with nextjs, markdown posts, tags, an rss feed and a dark theme. do everything, ask me before big decisions")
	for i := 0; w.Now().Sub(t0) < 4*hr; i++ {
		a.Agent(R(20*sec), W(time.Duration(30+a.rng.IntN(60))*sec, fmt.Sprintf("app/p%d.tsx", i%12)), Pass(30*sec, cmdNpmBuild))
		a.Prompt(time.Duration(30+a.rng.IntN(90))*sec, stamps[i%len(stamps)])
		if i%10 == 9 {
			a.Commit()
		}
	}
	a.End()
	work := w.Now().Sub(t0)
	a.Wait(3 * mn)
	o := outcome(d.Submit(ProjectKey("stamper", "blog"), SubmitOpts{}))
	o.Work, o.Effort = work.Hours(), "4 h at the keyboard (real presence, no struggle)"
	return o, nil
}

func atkAlarmClock(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("sleeper", "laptop")
	if err != nil {
		return nil, err
	}
	root := d.Project("", "rpg")
	a := NewActor(d, seed).In(root)
	t0 := w.Now()
	present := time.Duration(0)
	a.Start()
	a.Prompt(2*mn, "build a turn-based rpg battle system in godot: party of 3, enemies, items, xp and levels; work for a long time before stopping")
	present += 2 * mn
	lines := []string{"keep going", "next feature", "continue please", "keep going, add more enemies", "next", "carry on with the item system"}
	for i := 0; w.Now().Sub(t0) < 6*hr; i++ {
		var steps []Step
		for k := 0; k < 14; k++ {
			steps = append(steps, W(time.Duration(50+a.rng.IntN(20))*sec, fmt.Sprintf("scripts/s%d.gd", (i*14+k)%30)))
		}
		steps = append(steps, Pass(30*sec, "godot --headless --run-tests"))
		a.Agent(steps...)
		back := time.Duration(45+a.rng.IntN(45)) * sec
		a.Wait(back)
		a.Send(lines[i%len(lines)])
		present += 30 * sec
	}
	a.End()
	a.Wait(3 * mn)
	o := outcome(d.Submit(ProjectKey("sleeper", "rpg"), SubmitOpts{}))
	o.Work, o.Effort = present.Hours(), "about 30 s at the keyboard per 15-min turn"
	return o, nil
}

// ---------------------------------------------------------------- server API abuse

func atkForgedBuckets(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("bucketeer", "laptop")
	if err != nil {
		return nil, err
	}
	const nProj = 50
	seq := int64(1)
	post := func(bs []ledger.Bucket, metas []api.ProjectMeta) (int, string) {
		seq += 10
		cp := chain.Checkpoint{Seq: seq, Hash: fmt.Sprintf("%064x", seq)}
		sig, _ := syncer.SignCheckpoint(d.Auth, cp)
		code, body := d.rawPost("/api/v1/sync", api.SyncReq{DeviceID: d.Auth.DeviceID, ClientVersion: "sim", ScoreVersion: ledger.Version,
			Checkpoint: cp, CheckpointSig: sig, Buckets: bs, Projects: metas})
		w.Advance(time.Second)
		return code, body
	}
	bucket := func(p int, start time.Time) ledger.Bucket {
		return ledger.Bucket{Project: fmt.Sprintf("github.com/bucketeer/p%02d", p), Start: start, CodingSeconds: 58, AINotCountedSeconds: 2, Prompts: 1}
	}
	total := func() float64 {
		t := 0.0
		for p := 0; p < nProj; p++ {
			if h := w.CompatHours("bucketeer", fmt.Sprintf("p%02d", p)); h > 0 {
				t += h
			}
		}
		return t
	}
	// 1) backfill: 8 h of a day three days ago, 60 s/min in each of 50 projects
	day := w.Now().Add(-3 * 24 * hr).Truncate(24 * hr).Add(9 * hr)
	var lastMsg string
	for p := 0; p < nProj; p++ {
		var bs []ledger.Bucket
		for m := 0; m < 8*60; m++ {
			bs = append(bs, bucket(p, day.Add(time.Duration(m)*mn)))
		}
		key := fmt.Sprintf("github.com/bucketeer/p%02d", p)
		_, lastMsg = post(bs, []api.ProjectMeta{{Key: key, Name: fmt.Sprintf("p%02d", p), Remote: "https://" + key}})
	}
	backfill := total()
	// 2) live: for 2 h, every 90 s, the last 90 s of minutes in every project
	t0 := w.Now()
	for w.Now().Sub(t0) < 2*hr {
		var bs []ledger.Bucket
		for p := 0; p < nProj; p++ {
			bs = append(bs, bucket(p, w.Now().Truncate(mn).Add(-mn)), bucket(p, w.Now().Truncate(mn)))
		}
		post(bs, nil)
		w.Advance(89 * sec)
	}
	o := &Outcome{Compat: total()}
	o.Credited = o.Compat
	o.Work, o.Copycat, o.Effort = 0.5, 0.05, "30 min: a script posting JSON with the device token"
	o.Notes = append(o.Notes, fmt.Sprintf("backfilled day: compat API shows %.1f h over 50 projects (last server message: %q); after 2 h of live posting: %.1f h; one project: %.1f h",
		backfill, lastMsg, o.Compat, w.CompatHours("bucketeer", "p00")))
	return o, nil
}
