package idle

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"seatime/pkg/event"
)

// Reading is the latest sample, for `status` and `doctor`.
type Reading struct {
	Src  string        `json:"src"`
	At   time.Time     `json:"at"`
	Idle time.Duration `json:"idle_ns"`
	OK   bool          `json:"ok"`
	Err  string        `json:"err,omitempty"`
}

// Recorder turns minute samples of a Source into idle events. A window is a
// run of ticks no more than MaxGap apart; a longer gap (sleep, shutdown,
// crash) or a clock that went backwards closes it, so no recorded span ever
// covers a sleep or runs backwards. Within a window, every stretch the OS
// says had no input and is at least MinNoInput long is recorded (it may
// start before the window: the OS counts from the last input). A window is
// written when it closes, after FlushActive if it saw input, or after
// FlushIdle otherwise (an unattended machine writes about one event an hour).
type Recorder struct {
	Src  Source
	Emit func(event.Event) error
	Now  func() time.Time

	// Active, when set, says a project is active now: Run then also samples
	// every SliceEvery and records, per minute, which 5 s slices saw input.
	Active func() bool
	// Presence, when set, names the device's presence level for an idle
	// record, given whether the idle timer answered (package presence).
	Presence func(timerOK bool) string
	// Front, when set, says at each 5 s slice whether the AI tool's own
	// window is in front (ok false: it cannot be told now). The minutes it
	// answered go out as FrontSlices.
	Front func(ctx context.Context) (front, ok bool)

	// Near is how long after the last input the 5 s tick goes on reading
	// the idle timer with no project active (Fast). Default 10 min.
	Near time.Duration

	MaxGap      time.Duration // default 90 s
	MinNoInput  time.Duration // default 60 s
	FlushActive time.Duration // default 10 min
	FlushIdle   time.Duration // default 60 min

	mu    sync.Mutex
	win   *window
	last  Reading
	meter meter
}

type window struct {
	from, last time.Time
	src        string
	noInput    [][2]int64
	sawInput   bool
	slices     map[int64]int64 // minute start (unix ms) -> 12-bit mask
	fronts     map[int64]int64 // minute start (unix ms) -> 12-bit mask of slices with the tool's window in front
	lastSlice  time.Time       // the previous slice sample
}

// SliceEvery is the slice width: twelve slices a minute.
const SliceEvery = 5 * time.Second

func (r *Recorder) defaults() {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.MaxGap == 0 {
		r.MaxGap = 90 * time.Second
	}
	if r.MinNoInput == 0 {
		r.MinNoInput = time.Minute
	}
	if r.FlushActive == 0 {
		r.FlushActive = 10 * time.Minute
	}
	if r.FlushIdle == 0 {
		r.FlushIdle = time.Hour
	}
	if r.Near == 0 {
		r.Near = 10 * time.Minute
	}
}

// Last returns the latest reading.
func (r *Recorder) Last() Reading {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Tick takes one sample. It reads the wall clock only (Round(0) drops the
// monotonic reading, which stops during sleep).
func (r *Recorder) Tick(ctx context.Context) {
	r.defaults()
	now := r.Now().Round(0).UTC()
	src := r.Src
	if src == nil {
		src = None{}
	}
	d, err := src.Idle(ctx)
	name := src.Name()
	if err != nil {
		name = "none"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = Reading{Src: name, At: now, Idle: d, OK: err == nil}
	if err != nil {
		r.last.Err = err.Error()
	} else {
		r.meter.add(now, d)
	}
	if w := r.win; w != nil && (now.Sub(w.last) > r.MaxGap || now.Before(w.last) || w.src != name) {
		r.flushLocked()
	}
	if r.win == nil {
		r.win = &window{from: now, last: now, src: name}
	}
	w := r.win
	w.last = now
	if err == nil {
		if d < 0 {
			d = 0
		}
		// not clipped to the window: "no input since X" is the OS's own
		// statement about the past, and it can only ever remove time
		from := now.Add(-d)
		if now.Sub(from) >= r.MinNoInput {
			w.addNoInput(from.UnixMilli(), now.UnixMilli())
		}
		if d < r.MaxGap {
			w.sawInput = true
		}
	}
	span := w.last.Sub(w.from)
	if span >= r.FlushIdle || span >= r.FlushActive && w.sawInput {
		r.flushLocked()
		last := r.win
		r.win = &window{from: now, last: now, src: name} // the next window continues seamlessly
		if last != nil {
			r.win.lastSlice = last.lastSlice
		}
	}
}

func (w *window) addNoInput(from, to int64) {
	if n := len(w.noInput); n > 0 && from <= w.noInput[n-1][1]+1000 {
		if from < w.noInput[n-1][0] {
			w.noInput[n-1][0] = from
		}
		if to > w.noInput[n-1][1] {
			w.noInput[n-1][1] = to
		}
		return
	}
	w.noInput = append(w.noInput, [2]int64{from, to})
}

// Slice takes one 5 s sample while a project is active. The minute it falls
// in is marked as sampled; the slice holding the last input, when that input
// came after the previous sample, is marked as having input. A sample that
// cannot be read marks nothing (that minute is then not measured, never
// "no input").
func (r *Recorder) Slice(ctx context.Context) { r.sample(ctx, true) }

// sample reads the idle timer once for the meter and, when record is set,
// for the window's slices.
func (r *Recorder) sample(ctx context.Context, record bool) {
	r.defaults()
	now := r.Now().Round(0).UTC()
	src := r.Src
	if src == nil {
		return
	}
	d, err := src.Idle(ctx)
	if err != nil {
		return
	}
	front, frontOK := false, false
	if record && r.Front != nil {
		front, frontOK = r.Front(ctx)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meter.add(now, d)
	if !record {
		return
	}
	w := r.win
	if w == nil || w.src != src.Name() {
		return // the minute tick opens windows; it comes within a minute
	}
	if w.slices == nil {
		w.slices = map[int64]int64{}
	}
	minute := func(t time.Time) int64 { return t.Truncate(time.Minute).UnixMilli() }
	if _, ok := w.slices[minute(now)]; !ok {
		w.slices[minute(now)] = 0
	}
	if frontOK {
		if w.fronts == nil {
			w.fronts = map[int64]int64{}
		}
		m := w.fronts[minute(now)]
		if front {
			m |= 1 << (now.Sub(now.Truncate(time.Minute)) / SliceEvery)
		}
		w.fronts[minute(now)] = m
	}
	input := now.Add(-max(d, 0))
	if !w.lastSlice.IsZero() && input.After(w.lastSlice) {
		if m, ok := w.slices[minute(input)]; ok {
			w.slices[minute(input)] = m | 1<<(input.Sub(input.Truncate(time.Minute))/SliceEvery)
		}
	}
	w.lastSlice = now
}

// Flush writes the open window (call on shutdown).
func (r *Recorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushLocked()
}

func (r *Recorder) flushLocked() {
	w := r.win
	r.win = nil
	if w == nil || !w.last.After(w.from) || r.Emit == nil {
		return
	}
	_ = r.Emit(r.record(w))
}

// Live is the open window as the idle record it will become, without
// writing it (nil when there is none yet): what the recorder knows now,
// for checks that cannot wait for the window to close.
func (r *Recorder) Live() *event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.win == nil {
		return nil
	}
	w := *r.win
	if w.lastSlice.After(w.last) { // slices run every 5 s, the minute tick less often
		w.last = w.lastSlice
	}
	if !w.last.After(w.from) {
		return nil
	}
	e := r.record(&w)
	return &e
}

func (r *Recorder) record(w *window) event.Event {
	e := event.Event{Kind: event.Idle, Harness: "seatime", TS: w.last,
		IdleSrc: w.src, WindowMS: w.last.Sub(w.from).Milliseconds(), NoInput: w.noInput}
	for m, mask := range w.slices {
		e.InputSlices = append(e.InputSlices, [2]int64{m, mask})
	}
	slices.SortFunc(e.InputSlices, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
	for m, mask := range w.fronts {
		e.FrontSlices = append(e.FrontSlices, [2]int64{m, mask})
	}
	slices.SortFunc(e.FrontSlices, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
	if r.Presence != nil {
		e.Presence = r.Presence(w.src != "none")
	}
	e.NoInput = slices.Clone(e.NoInput)
	return e
}

// Run ticks every interval until ctx ends, then flushes. Every SliceEvery
// it also runs Fast.
func (r *Recorder) Run(ctx context.Context, every time.Duration) {
	r.Tick(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	fast := time.NewTicker(SliceEvery)
	defer fast.Stop()
	for {
		select {
		case <-ctx.Done():
			r.Flush()
			return
		case <-t.C:
			r.Tick(ctx)
		case <-fast.C:
			r.Fast(ctx)
		}
	}
}
