// Package presence records how the person's presence was measured at a
// prompt (the person's trail), as numbers and yes/no only:
// milliseconds since the last keyboard or mouse input, seconds since keys
// reached the tool's terminal, and, when switched on, whether the tool's own
// window was in front. It never records a key, a window title or the name
// of any program, and nothing it records changes hours.
//
// Every OS call sits behind an interface (Idle, Terminal, Windows, Tmux) so
// the logic is tested with fakes; the real ones are in the per-OS files.
package presence

import (
	"context"
	"os"
	"slices"
	"time"
)

// The levels, by device. A page names the level and never mixes them.
const (
	InputFront   = "input+front window"
	InputOnly    = "input only"
	TerminalOnly = "terminal only"
	NotMeasured  = "not measured"
)

// The front-window answers.
const (
	FrontYes  = "yes"
	FrontNo   = "no"
	FrontNone = "no window attached"
)

// TerminalStep is the resolution of a terminal's idle time: the kernel
// updates a terminal's access time at most every 8 seconds.
const TerminalStep = 8 * time.Second

// Idle is the OS idle timer: time since the last keyboard or mouse input.
type Idle interface {
	Idle(ctx context.Context) (time.Duration, error)
}

// Terminal is the idle time of the terminal this process's tool runs in
// (what `w` prints).
type Terminal interface {
	Idle(ctx context.Context) (time.Duration, error)
}

// Windows answers which process owns the front window, and parent
// processes, so the front window can be compared with this process's own
// chain. Only process ids pass through it, never names.
type Windows interface {
	FrontPID(ctx context.Context) (int, error)
	Parent(pid int) (int, error)
}

// Tmux lists the processes of the clients attached to this pane's session.
type Tmux interface {
	Clients(ctx context.Context) ([]int, error)
}

// Probe is what a prompt's facts are read from.
type Probe struct {
	Getenv   func(string) string
	Idle     Idle     // nil: no idle timer
	Terminal Terminal // nil: no terminal reading
	Windows  Windows  // nil: front window not supported here
	Tmux     Tmux     // nil: tmux not asked
	FrontOn  bool     // the front-window switch (off by default)
	Self     int      // this process's id
}

// Facts are a prompt's presence facts.
type Facts struct {
	Presence      string
	InputBeforeMS *int64
	TerminalIdleS *int
	FrontWindow   string
}

// Remote reports whether this process was reached over SSH.
func (p Probe) Remote() bool {
	return p.Getenv("SSH_CONNECTION") != "" || p.Getenv("SSH_CLIENT") != "" || p.Getenv("SSH_TTY") != ""
}

// AtPrompt reads the facts for a prompt sent now.
//
// The rules that keep them honest:
//   - Over SSH the machine's own idle timer is not used: on a Mac it
//     truthfully says "no input" while the person types on another
//     computer. Only the terminal is read: "terminal only".
//   - Without a desktop idle timer, the terminal is read: "terminal only".
//   - A reading that fails leaves the field out; with nothing read the level
//     is "not measured", never "no input".
func (p Probe) AtPrompt(ctx context.Context) Facts {
	var f Facts
	if !p.Remote() && p.Idle != nil {
		if d, err := p.Idle.Idle(ctx); err == nil {
			ms := max(d, 0).Milliseconds()
			f.InputBeforeMS = &ms
		}
	}
	if f.InputBeforeMS == nil && p.Terminal != nil {
		if d, err := p.Terminal.Idle(ctx); err == nil {
			s := int(max(d, 0) / TerminalStep * TerminalStep / time.Second)
			f.TerminalIdleS = &s
		}
	}
	switch {
	case f.InputBeforeMS != nil:
		f.Presence = InputOnly
		if p.FrontOn && p.Windows != nil {
			if f.FrontWindow = p.front(ctx); f.FrontWindow != "" {
				f.Presence = InputFront
			}
		}
	case f.TerminalIdleS != nil:
		f.Presence = TerminalOnly
	default:
		f.Presence = NotMeasured
	}
	return f
}

// Level is the device's level when no prompt is being read (the daemon's
// idle records, `seatime doctor`): input only when the idle timer answers
// (input+front window when the switch is on and supported), terminal only
// over SSH or without a desktop, else not measured.
func Level(idleOK, remote, frontOn, frontSupported bool) string {
	switch {
	case remote:
		return TerminalOnly
	case idleOK && frontOn && frontSupported:
		return InputFront
	case idleOK:
		return InputOnly
	}
	return NotMeasured
}

// front answers whether the front window belongs to this process's chain:
// the terminal or app the tool runs in is an ancestor of this process.
// Inside tmux the attached clients stand in for this process, and with none
// attached nothing of the tool is on any screen. "" when it cannot be told.
func (p Probe) front(ctx context.Context) string {
	ours := []int{p.Self}
	if p.Getenv("TMUX") != "" {
		if p.Tmux == nil {
			return ""
		}
		cs, err := p.Tmux.Clients(ctx)
		if err != nil {
			return ""
		}
		if len(cs) == 0 {
			return FrontNone
		}
		ours = cs
	}
	front, err := p.Windows.FrontPID(ctx)
	if err != nil || front <= 0 {
		return ""
	}
	for _, pid := range ours {
		for i := 0; pid > 1 && i < 64; i++ {
			if pid == front {
				return FrontYes
			}
			if pid, err = p.Windows.Parent(pid); err != nil {
				break
			}
		}
	}
	return FrontNo
}

// FrontSupported reports whether this machine can say which window is in
// front (no on Wayland and on systems without a reader).
func FrontSupported(getenv func(string) string) bool { return windows(getenv) != nil }

// SystemWindows is this machine's front-window reader, or nil where the
// OS gives none (Wayland, systems without a reader).
func SystemWindows() Windows { return windows(os.Getenv) }

// Chain is the process ids whose window counts as the tool's own: this
// process and its ancestors (inside tmux, the attached clients and
// theirs). Process ids only, never names. nil when it cannot be told.
func (p Probe) Chain(ctx context.Context) []int {
	if p.Windows == nil {
		return nil
	}
	ours := []int{p.Self}
	if p.Getenv("TMUX") != "" {
		if p.Tmux == nil {
			return nil
		}
		cs, err := p.Tmux.Clients(ctx)
		if err != nil || len(cs) == 0 {
			return nil
		}
		ours = cs
	}
	var out []int
	for _, pid := range ours {
		for i := 0; pid > 1 && i < 64; i++ {
			out = append(out, pid)
			var err error
			if pid, err = p.Windows.Parent(pid); err != nil {
				break
			}
		}
	}
	return out
}

// InChain reports whether the front window's process is in chain.
func InChain(front int, chain []int) bool {
	return front > 1 && slices.Contains(chain, front)
}
