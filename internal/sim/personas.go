package sim

import (
	"fmt"
	"time"
)

// Commands the agent or the human runs. runclass classifies them as the real
// adapters do (test / build / exec).
const (
	cmdGoTest   = "go test ./..."
	cmdNpmBuild = "npm run build"
	cmdNpmTest  = "npm test"
	cmdPySnake  = "python3 snake.py"
	cmdPipInst  = "pip install pygame"
)

// single runs body for one person on one device and project, lets the
// daemon sync, and submits the project.
func single(w *World, seed uint64, user, proj string, body func(a *Actor)) (*Outcome, error) {
	d, err := w.NewDevice(user, "laptop")
	if err != nil {
		return nil, err
	}
	a := NewActor(d, seed).In(d.Project("", proj))
	body(a)
	work := a.WorkTime()
	a.Wait(3 * mn) // the daemon syncs; then the person runs `seatime submit`
	s := d.Submit(ProjectKey(user, proj), SubmitOpts{})
	o := &Outcome{Subs: []*SubResult{s}, Credited: s.Hours(), Presence: s.Scored.Seconds() / 3600, Work: work.Hours(),
		Compat: w.CompatHours(user, proj)}
	return o, nil
}

// HonestPersonas returns the honest users, each with the fraction of their
// real work time a fair count would credit.
func HonestPersonas() []*Persona {
	return []*Persona{
		{Name: "beginner_python", Honest: true, Gate: true, Frac: [2]float64{0.55, 1.0},
			Desc: "13-year-old's first Python game with Claude Code: vague prompts (\"it doesnt work\", \"error\"), Python tracebacks, steady progress, hand tweaks, one commit.",
			Run: func(w *World, seed uint64) (*Outcome, error) {
				return single(w, seed, "kid", "snake", beginnerPython)
			}},
		{Name: "beginner_react", Honest: true, Gate: false, Frac: [2]float64{0.55, 0.95},
			Desc: "First React app (todo list), empty history, errors pasted from the console, hand edits, two commits.",
			Run: func(w *World, seed uint64) (*Outcome, error) {
				return single(w, seed, "newbie", "todo", beginnerReact)
			}},
		// Not gated: its low fair range came from the comfort-zone discount,
		// which the one-page rule drops on purpose; measured
		// at 0.64 of 0.95 h under ledger/2.
		{Name: "expert_boilerplate", Honest: true, Gate: false, Frac: [2]float64{0.12, 0.40},
			Desc: "Experienced dev doing CRUD #11-20 with the same template; tests pass first try. Fair credit is low (comfort zone).",
			Run: func(w *World, seed uint64) (*Outcome, error) {
				return single(w, seed, "pro", "crud", expertBoilerplate)
			}},
		{Name: "free_model_retries", Honest: true, Gate: true, Frac: [2]float64{0.5, 1.0},
			Desc: "Same pagination task with a free/weak model: 6 failures, agent retries inside its turns, a hand fix.",
			Run: func(w *World, seed uint64) (*Outcome, error) {
				return single(w, seed, "freebie", "posts", freeModelRetries)
			}},
		// Not gated: a strong model's long solo turns earn nothing past 12
		// minutes without an act (rule 4), so this user lands at 0.08 of a
		// 0.20 h task; free_model_retries vs opus_user below still guards
		// pay-to-win.
		{Name: "opus_user", Honest: true, Gate: false, Frac: [2]float64{0.5, 1.0},
			Desc: "Same pagination task with a strong model: right first time, one follow-up, commit.",
			Run: func(w *World, seed uint64) (*Outcome, error) {
				return single(w, seed, "opusfan", "posts", opusTask)
			}},
		{Name: "hand_coder_editor_only", Honest: true, Gate: true, Frac: [2]float64{0.45, 1.0}, // heartbeat/1 counts hand edits as Hackatime does
			Desc: "Writes Rust by hand in VS Code with the WakaTime plugin feeding SeaTime; no AI, no terminal hook; commits.",
			Run: func(w *World, seed uint64) (*Outcome, error) {
				return single(w, seed, "crab", "rustbox", handCoder)
			}},
		{Name: "parallel_agents", Honest: true, Gate: false, Frac: [2]float64{0.55, 1.0},
			Desc: "Steers 4 Claude Code sessions in one repo, specific prompts to each, reviews and commits.",
			Run: func(w *World, seed uint64) (*Outcome, error) {
				return single(w, seed, "juggler", "dash", parallelAgents)
			}},
		{Name: "multi_day_breaks", Honest: true, Gate: true, Frac: [2]float64{0.5, 1.0},
			Desc: "Same project over 3 evenings (about 1.5 h each, a 20 min snack break), one submission at the end.",
			Run: func(w *World, seed uint64) (*Outcome, error) {
				return single(w, seed, "steady", "weather", multiDay)
			}},
		{Name: "two_projects_one_day", Honest: true, Gate: true, Frac: [2]float64{0.5, 1.0},
			Desc: "Alternates between a school website and a game in 20-minute blocks on one laptop, switching windows within seconds; submits both projects (to two programs).",
			Run:  func(w *World, seed uint64) (*Outcome, error) { return twoProjects(w, seed) }},
		{Name: "two_devices", Honest: true, Gate: true, Frac: [2]float64{0.5, 1.0},
			Desc: "School laptop in the morning, home desktop in the evening, same repo (pulls own commits), one submission per device; the program adds them.",
			Run:  func(w *World, seed uint64) (*Outcome, error) { return twoDevices(w, seed) }},
	}
}

func pyTB(last string) string {
	return "Traceback (most recent call last):\n  File \"snake.py\", line 31, in <module>\n" + last + "\nexit code 1"
}

func beginnerPython(a *Actor) {
	a.Start()
	a.Prompt(90*sec, "make a snake game")
	a.Agent(W(60*sec, "snake.py"), Fail(20*sec, cmdPySnake, pyTB("ModuleNotFoundError: No module named 'pygame'")))
	a.Prompt(mn, "it doesnt work")
	a.Agent(R(20*sec), Pass(40*sec, cmdPipInst), Pass(20*sec, cmdPySnake))
	a.Prompt(2*mn, "the snake doesnt move")
	a.Agent(W(40*sec, "snake.py"), Fail(20*sec, cmdPySnake, pyTB("NameError: name 'direction' is not defined")))
	a.Prompt(mn, "still doesnt work")
	a.Agent(W(30*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	a.Prompt(2*mn, "it moves now but it goes off the screen")
	a.Agent(W(40*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	a.Prompt(90*sec, "can it come back on the other side")
	a.Agent(W(30*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	a.WaitJ(mn)
	a.Typing("snake.py", 3*mn, 0.5, 50*sec)
	a.Prompt(mn, "add food")
	a.Agent(W(40*sec, "snake.py"), Fail(20*sec, cmdPySnake, pyTB("TypeError: 'tuple' object does not support item assignment")))
	a.Prompt(mn, "error")
	a.Agent(W(30*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	a.Prompt(2*mn, "the snake doesnt get longer when it eats")
	a.Agent(W(40*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	a.Prompt(2*mn, "how does the snake list thing work")
	a.Agent(R(30 * sec))
	a.WaitJ(2 * mn)
	a.Typing("snake.py", 4*mn, 0.5, 50*sec)
	a.Prompt(mn, "now add a score")
	a.Agent(W(40*sec, "snake.py"), Fail(20*sec, cmdPySnake, pyTB("pygame.error: font not initialized")))
	a.Prompt(mn, "it crashed")
	a.Agent(W(30*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	a.Prompt(2*mn, "game over when it hits itself")
	a.Agent(W(40*sec, "snake.py"), Pass(20*sec, cmdPySnake))
	a.WaitJ(2 * mn)
	a.Commit()
	a.End()
}

func beginnerReact(a *Actor) {
	a.Start()
	a.Prompt(2*mn, "i want to make a todo list website where i can add and delete tasks. i have never used react before, can you set it up and explain the files")
	a.Agent(R(20*sec), W(40*sec, "src/App.jsx"), W(30*sec, "src/App.css"), Pass(20*sec, cmdNpmBuild))
	a.Prompt(3*mn, "it doesnt work")
	a.Agent(R(30*sec), Pass(30*sec, cmdNpmBuild))
	a.Prompt(90*sec, "when i click add nothing happens and the console says Uncaught TypeError: Cannot read properties of undefined (reading 'push') at handleAdd (App.jsx:14:11)")
	a.Agent(R(15*sec), W(40*sec, "src/App.jsx"), Pass(20*sec, cmdNpmBuild))
	a.Prompt(2*mn, "ok that works now. why do we use useState instead of a normal variable for the list?")
	a.Agent(R(30 * sec))
	a.WaitJ(90 * sec)
	a.Typing("src/App.jsx", 4*mn, 1, mn)
	a.Prompt(mn, "how do i make the delete button only show up when i hover over a task")
	a.Agent(W(40*sec, "src/App.css"), W(20*sec, "src/App.jsx"))
	a.Prompt(2*mn, "its not showing up at all now")
	a.Agent(R(20*sec), W(30*sec, "src/App.css"))
	a.Prompt(mn, "still not working")
	a.Agent(R(20*sec), W(20*sec, "src/App.css"))
	a.Typing("src/App.css", 3*mn, 0.7, 40*sec)
	a.Prompt(mn, "i changed the opacity thing and now it shows but the whole list jumps when i hover. here is my css: .task:hover .delete { display: block; } .delete { display: none; margin-left: 8px }")
	a.Agent(W(30*sec, "src/App.css"))
	a.Prompt(2*mn, "nice that fixed it!! now i want to save the todos so they dont disappear when i refresh the page")
	a.Agent(W(60*sec, "src/App.jsx"), Fail(20*sec, cmdNpmBuild, `✘ [ERROR] Expected ")" but found "localStorage"`+"\n    src/App.jsx:22:9:"), W(30*sec, "src/App.jsx"), Pass(20*sec, cmdNpmBuild))
	a.Prompt(3*mn, "it saves but when i reload i get a blank white page")
	a.Agent(R(20*sec), Pass(20*sec, cmdNpmBuild))
	a.Prompt(90*sec, "the console says SyntaxError: Unexpected token u in JSON at position 0 at JSON.parse (<anonymous>) at App (App.jsx:6:25)")
	a.Agent(W(30*sec, "src/App.jsx"), Pass(20*sec, cmdNpmBuild))
	a.Prompt(2*mn, "it works!! can you explain what JSON.parse does and why it broke when there was nothing saved yet")
	a.Agent(R(30 * sec))
	a.WaitJ(2 * mn)
	a.Typing("src/App.jsx", 5*mn, 0.8, mn)
	a.Commit()
	a.Prompt(mn, "now i want a checkbox to mark a task as done and cross it out")
	a.Agent(W(40*sec, "src/App.jsx"), W(20*sec, "src/App.css"), Pass(20*sec, cmdNpmBuild))
	a.Prompt(2*mn, "when i check one box all of them get checked")
	a.Agent(R(20*sec), W(30*sec, "src/App.jsx"), Pass(20*sec, cmdNpmBuild))
	a.Prompt(2*mn, "that worked but i dont understand why using the id fixed it, the index was the same thing right?")
	a.Agent(R(30 * sec))
	a.WaitJ(2 * mn)
	a.Typing("src/App.jsx", 3*mn, 0.6, mn)
	a.Commit()
	a.End()
}

var crudEnts = []string{"customers", "products", "orders", "invoices", "suppliers", "warehouses", "shipments", "employees", "categories", "coupons"}

func crudPrompt(e string) string {
	return "add CRUD endpoints for " + e + " following the existing pattern: gorm model, repository, handler with validation, routes and table tests"
}

func expertBoilerplate(a *Actor) {
	var hist []string
	for _, e := range crudEnts {
		hist = append(hist, crudPrompt(e))
	}
	a.Start()
	for i, e := range crudEnts {
		a.Prompt(50*sec, crudPrompt(e))
		a.Agent(R(20*sec), W(50*sec, "internal/"+e+"/model.go"), W(60*sec, "internal/"+e+"/handler.go"), W(60*sec, "internal/"+e+"/handler_test.go"), Pass(40*sec, cmdGoTest))
		a.WaitJ(40 * sec)
		if i%2 == 1 {
			a.Commit()
		}
	}
	a.End()
}

func opusTask(a *Actor) {
	a.Model = "claude-opus-4-5"
	a.Start()
	a.Prompt(2*mn, "add cursor pagination to GET /posts: opaque base64 cursor of (created_at, id), limit param capped at 100, return next_cursor, plus table tests")
	a.Agent(R(20*sec), W(60*sec, "posts/handler.go"), W(60*sec, "posts/handler_test.go"), Pass(40*sec, cmdGoTest))
	a.WaitJ(2 * mn)
	a.Prompt(mn, "return next_cursor as null on the last page and add a test for an empty table")
	a.Agent(W(40*sec, "posts/handler.go"), W(30*sec, "posts/handler_test.go"), Pass(40*sec, cmdGoTest))
	a.WaitJ(mn)
	a.Commit()
	a.End()
}

func freeModelRetries(a *Actor) {
	a.Model = "free-small"
	inner := map[string]bool{}
	turn := func(steps ...Step) {
		var out []Step
		for _, st := range steps {
			if st.Exit != 0 && !inner[st.Output] {
				inner[st.Output] = true
				out = append(out, W(30*sec, "posts/handler.go"), Fail(30*sec, st.Cmd, st.Output), W(30*sec, "posts/handler.go"), Fail(30*sec, st.Cmd, st.Output))
			}
			out = append(out, st)
		}
		a.Agent(out...)
	}
	a.Start()
	a.Prompt(2*mn, "add cursor pagination to GET /posts: opaque base64 cursor of (created_at, id), limit param capped at 100, return next_cursor, plus table tests")
	turn(R(20*sec), W(60*sec, "posts/handler.go"), W(60*sec, "posts/handler_test.go"), Fail(40*sec, cmdGoTest, "# app/posts\nposts/handler.go:41:12: undefined: encodeCursor\nFAIL\tapp/posts [build failed]"))
	a.Prompt(mn, "the test fails with undefined: encodeCursor, you never wrote that function. write encodeCursor and decodeCursor")
	turn(W(60*sec, "posts/handler.go"), Fail(40*sec, cmdGoTest, "panic: runtime error: index out of range [3] with length 3\n\ngoroutine 7 [running]:\nexit status 2"))
	a.Prompt(90*sec, "now it panics with index out of range on the last page. the limit+1 trick is off by one when there are fewer rows than limit")
	turn(W(50*sec, "posts/handler.go"), Fail(40*sec, cmdGoTest, "--- FAIL: TestListPosts/full_page (0.00s)\n    handler_test.go:52: expected 10 items, got 11"))
	a.Prompt(40*sec, "try again")
	turn(W(50*sec, "posts/handler.go"), Fail(40*sec, cmdGoTest, "--- FAIL: TestListPosts/full_page (0.00s)\n    handler_test.go:52: expected 10 items, got 11"))
	a.WaitJ(2 * mn)
	a.Prompt(2*mn, "still returns 11. slice rows[:limit] before building the response; the extra row is only there to compute hasMore, it must not be returned")
	turn(W(40*sec, "posts/handler.go"), Pass(40*sec, cmdGoTest))
	a.WaitJ(mn)
	a.Prompt(mn, "return next_cursor as null on the last page and add a test for an empty table")
	turn(W(40*sec, "posts/handler.go"), W(30*sec, "posts/handler_test.go"), Fail(40*sec, cmdGoTest, "--- FAIL: TestListPosts/empty (0.00s)\n    handler_test.go:77: expected nil next_cursor, got \"\""))
	a.Prompt(mn, "the empty case still returns an empty string")
	turn(W(40*sec, "posts/handler.go"), Fail(40*sec, cmdGoTest, "--- FAIL: TestListPosts/empty (0.00s)\n    handler_test.go:77: expected nil next_cursor, got \"\""))
	a.WaitJ(mn)
	a.Typing("posts/handler.go", 3*mn, 0.7, 50*sec)
	a.HumanRun(30*sec, cmdGoTest, 0, "")
	a.Commit()
	a.End()
}

func handCoder(a *Actor) {
	files := []string{"src/main.rs", "src/grid.rs", "src/render.rs", "src/input.rs"}
	for i, f := range files {
		a.Typing(f, time.Duration(12+3*i)*mn, 0.9, 50*sec)
		a.WaitJ(90 * sec) // reads docs
		if i%2 == 1 {
			a.Commit()
		}
	}
	a.Typing("src/main.rs", 8*mn, 0.6, 40*sec)
	a.Commit()
}

func parallelAgents(a *Actor) {
	root := a.Root
	sess := make([]*Actor, 4)
	for i := range sess {
		s := *a
		s.In(root)
		sess[i] = &s
		s.Start()
	}
	tasks := [][]string{
		{"add a websocket endpoint that streams build status updates to the dashboard, reuse the existing hub", "the stream drops after 60s behind caddy, add ping frames every 20s and handle pong timeouts"},
		{"write a migration that adds a builds.duration_ms column and backfill it from started_at and finished_at", "the backfill locks the table for too long on prod-size data, batch it 5000 rows at a time"},
		{"make the dashboard chart show p50 and p95 build time per day with a toggle between them", "p95 line is flat because the query groups by hour, group by day in the user's timezone"},
		{"add a flaky-test detector: same test failing then passing on the same commit within 24h", "it flags tests that were fixed in the next commit, only count reruns on the identical sha"},
	}
	for round := 0; round < 2; round++ {
		for i, s := range sess { // prompts go out one after another...
			s.Prompt(time.Duration(70+20*i)*sec, tasks[i][round])
		}
		for i, s := range sess { // ...while the agents work in parallel
			s.Agent(R(15*sec), W(20*sec, fmt.Sprintf("pkg/f%d/a.go", i)), Pass(20*sec, cmdGoTest))
		}
		a.WaitJ(4 * mn) // reviews the diffs
		a.Typing("pkg/f0/a.go", 4*mn, 0.6, mn)
		a.HumanRun(30*sec, cmdGoTest, 0, "")
		a.Commit()
	}
	for _, s := range sess {
		s.End()
	}
}

func multiDay(a *Actor) {
	for day := 0; day < 3; day++ {
		a.Start()
		a.Prompt(2*mn, []string{
			"build a flask app that shows the 5 day forecast for a city using the open-meteo api, with a search box",
			"add caching so the same city isn't fetched more than once every 10 minutes, use a dict with timestamps",
			"add a chart of the hourly temperature with chart.js and a toggle for celsius and fahrenheit",
		}[day])
		a.Agent(R(20*sec), W(60*sec, "app.py"), W(40*sec, "templates/index.html"), Fail(30*sec, "pytest", "E   KeyError: 'hourly'\nFAILED tests/test_app.py::test_forecast"))
		a.Prompt(2*mn, []string{
			"the test fails with KeyError hourly, the api only returns hourly when you ask for it in the params",
			"the cache test fails because time.time is real, how do i fake the clock in pytest",
			"the toggle converts the numbers but the axis label still says celsius",
		}[day])
		a.Agent(W(40*sec, "app.py"), Pass(30*sec, "pytest"))
		a.WaitJ(2 * mn)
		a.Typing("app.py", 10*mn, 0.7, mn)
		a.HumanRun(30*sec, "pytest", 0, "")
		a.Commit()
		a.AwayFor(20 * mn) // snack
		a.Prompt(3*mn, []string{
			"show an error message instead of a 500 page when the city isn't found",
			"add a test for the cache expiry at exactly 10 minutes",
			"make the page work on a phone, the chart overflows",
		}[day])
		a.Agent(W(40*sec, "app.py"), W(30*sec, "templates/index.html"), Pass(30*sec, "pytest"))
		a.WaitJ(2 * mn)
		a.Typing("templates/index.html", 15*mn, 0.6, mn)
		a.Commit()
		a.End()
		if day < 2 {
			now := a.D.W.Now()
			a.AwayFor(time.Date(now.Year(), now.Month(), now.Day()+1, 17, 0, 0, 0, time.UTC).Sub(now)) // next evening
		}
	}
}

func twoProjects(w *World, seed uint64) (*Outcome, error) {
	d, err := w.NewDevice("switcher", "laptop")
	if err != nil {
		return nil, err
	}
	site := NewActor(d, seed).In(d.Project("", "school-site"))
	game := NewActor(d, seed+1).In(d.Project("", "platformer"))
	t0 := w.Now()
	siteBlock := func(i int) {
		site.Start()
		site.Prompt(90*sec, []string{
			"make the history project page show a timeline of the french revolution with dates on the left and events on the right",
			"the timeline overlaps on my phone, stack the dates above the events under 600px wide",
			"add a quiz at the bottom with 5 questions and show the score when you press submit",
		}[i])
		site.Agent(R(20*sec), W(50*sec, "timeline.html"), W(30*sec, "style.css"))
		site.WaitJ(mn)
		site.Typing("timeline.html", 8*mn, 0.8, mn)
		site.Prompt(mn, []string{"the dates are not lined up with the events, some events are two lines long", "the quiz button does nothing on safari", "the score says NaN out of 5"}[i])
		site.Agent(W(40*sec, "style.css"))
		site.Typing("style.css", 5*mn, 0.6, mn)
		site.Commit()
		site.End()
	}
	gameBlock := func(i int) {
		game.Start()
		game.Prompt(90*sec, []string{
			"add a double jump to the player in phaser, only once until you touch the ground again",
			"enemies walk off ledges, make them turn around at the edge of a platform",
			"add a coin counter in the top left that stays on screen when the camera moves",
		}[i])
		game.Agent(R(20*sec), W(60*sec, "src/player.js"), Fail(20*sec, cmdNpmTest, errOut("TypeError: Cannot read properties of undefined (reading 'body')")))
		game.Prompt(90*sec, []string{"the test says body is undefined, the player sprite is created after the jump handler", "they still fall off the right edge only", "the counter moves with the camera"}[i])
		game.Agent(W(40*sec, "src/player.js"), Pass(20*sec, cmdNpmTest))
		game.WaitJ(mn)
		game.Typing("src/player.js", 7*mn, 0.7, mn)
		game.Commit()
		game.End()
	}
	for i := 0; i < 3; i++ {
		siteBlock(i)
		game.WaitJ(8 * sec)
		gameBlock(i)
		site.WaitJ(8 * sec)
	}
	work := w.Now().Sub(t0)
	w.Advance(3 * mn)
	s1 := d.Submit(ProjectKey("switcher", "school-site"), SubmitOpts{})
	s2 := d.Submit(ProjectKey("switcher", "platformer"), SubmitOpts{})
	return &Outcome{Subs: []*SubResult{s1, s2}, Credited: s1.Hours() + s2.Hours(), Presence: (s1.Scored.Seconds() + s2.Scored.Seconds()) / 3600,
		Work: work.Hours(), Compat: -1}, nil
}

func twoDevices(w *World, seed uint64) (*Outcome, error) {
	lap, err := w.NewDevice("duo", "school-laptop")
	if err != nil {
		return nil, err
	}
	desk, err := w.NewDevice("duo", "home-desktop")
	if err != nil {
		return nil, err
	}
	key := ProjectKey("duo", "chess")
	a := NewActor(lap, seed).In(lap.Project("", "chess"))
	a.Start()
	a.Prompt(2*mn, "set up a typescript chess board with drag and drop pieces, no engine yet, just legal moves for pawns and knights")
	a.Agent(R(20*sec), W(60*sec, "src/board.ts"), W(40*sec, "src/moves.ts"), Fail(30*sec, cmdNpmTest, errOut("TypeError: Cannot read properties of undefined (reading 'color')")))
	a.Prompt(2*mn, "the move test fails because empty squares are undefined not null, make the board a fixed 64 array of Piece | null")
	a.Agent(W(40*sec, "src/board.ts"), Pass(30*sec, cmdNpmTest))
	a.WaitJ(2 * mn)
	a.Typing("src/moves.ts", 12*mn, 0.7, mn)
	a.Commit()
	a.End()
	work := a.WorkTime()
	firstCommitAt := lap.W.Now()

	a.AwayFor(6 * hr) // school, bus, dinner
	b := NewActor(desk, seed+1).In(desk.Project("", "chess"))
	b.ResetWork()
	// the desktop's git scan sees the laptop's commit after `git pull` (same author email)
	desk.Commit(b.Root, firstCommitAt.Add(-20*sec), 40, 2)
	b.Start()
	b.Prompt(3*mn, "add castling: king and rook unmoved, squares between empty and not attacked")
	b.Agent(R(20*sec), W(60*sec, "src/moves.ts"), Fail(30*sec, cmdNpmTest, errOut("AssertionError: expected castling to be illegal through check")))
	b.Prompt(2*mn, "castling through check is allowed, isAttacked only looks at the king square, check the two squares it passes too")
	b.Agent(W(40*sec, "src/moves.ts"), Pass(30*sec, cmdNpmTest))
	b.WaitJ(2 * mn)
	b.Typing("src/moves.ts", 15*mn, 0.6, mn)
	b.Commit()
	b.End()
	work += b.WorkTime()
	b.Wait(3 * mn)
	s1 := lap.Submit(key, SubmitOpts{})
	s2 := desk.Submit(key, SubmitOpts{})
	return &Outcome{Subs: []*SubResult{s1, s2}, Credited: s1.Hours() + s2.Hours(),
		Presence: (s1.Scored.Seconds() + s2.Scored.Seconds()) / 3600, Work: work.Hours(), Compat: w.CompatHours("duo", "chess")}, nil
}
