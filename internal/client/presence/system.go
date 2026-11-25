package presence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"seatime/internal/client/idle"
)

// ErrUnsupported: this OS or session cannot answer.
var ErrUnsupported = errors.New("not supported here")

// run runs a command with a short timeout and returns its stdout.
type run func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// System is the Probe for this process on this machine: the OS idle timer
// (the same sources the daemon reads), the tool's terminal, and, when the
// switch is on, the front window and tmux.
func System(frontOn bool) Probe {
	p := Probe{Getenv: os.Getenv, Self: os.Getpid(), FrontOn: frontOn, Terminal: terminal()}
	if src := idle.Default(); src.Name() != "none" {
		p.Idle = src
	}
	if frontOn {
		p.Windows = windows(os.Getenv)
		p.Tmux = tmuxClients{run: execRun}
	}
	return p
}

// tmuxClients asks tmux for the clients attached to this pane's session.
type tmuxClients struct{ run run }

func (t tmuxClients) Clients(ctx context.Context) ([]int, error) {
	sess, err := t.run(ctx, "tmux", "display-message", "-p", "#{session_name}")
	if err != nil {
		return nil, fmt.Errorf("tmux: %w", err)
	}
	out, err := t.run(ctx, "tmux", "list-clients", "-t", strings.TrimSpace(string(sess)), "-F", "#{client_pid}")
	if err != nil {
		return nil, fmt.Errorf("tmux: %w", err)
	}
	return ParseInts(out), nil
}

// ParseInts reads one number per line, skipping anything else.
func ParseInts(out []byte) []int {
	var ns []int
	for _, l := range strings.Split(string(out), "\n") {
		if n, err := strconv.Atoi(strings.TrimSpace(l)); err == nil {
			ns = append(ns, n)
		}
	}
	return ns
}

// ParseXpropActive reads the window id from
// `xprop -root _NET_ACTIVE_WINDOW` ("... window id # 0x3a00007").
func ParseXpropActive(out []byte) (string, error) {
	s := strings.TrimSpace(string(out))
	i := strings.LastIndex(s, "# ")
	if i < 0 {
		return "", fmt.Errorf("xprop: unexpected %q", s)
	}
	id := strings.TrimSpace(strings.SplitN(s[i+2:], ",", 2)[0])
	if !strings.HasPrefix(id, "0x") || id == "0x0" {
		return "", fmt.Errorf("xprop: no active window (%q)", id)
	}
	return id, nil
}

// ParseXpropPID reads `xprop -id W _NET_WM_PID` ("_NET_WM_PID(CARDINAL) = 1234").
func ParseXpropPID(out []byte) (int, error) {
	s := strings.TrimSpace(string(out))
	i := strings.LastIndex(s, "=")
	if i < 0 {
		return 0, fmt.Errorf("xprop: no pid in %q", s)
	}
	return strconv.Atoi(strings.TrimSpace(s[i+1:]))
}

// ParseLsappinfoPID reads `lsappinfo info -only pid ASN` (`"pid"=1234`).
func ParseLsappinfoPID(out []byte) (int, error) {
	s := strings.TrimSpace(string(out))
	i := strings.LastIndex(s, "=")
	if i < 0 {
		return 0, fmt.Errorf("lsappinfo: no pid in %q", s)
	}
	return strconv.Atoi(strings.TrimSpace(s[i+1:]))
}

// ParseProcStatPPID reads the parent id from /proc/<pid>/stat
// ("1234 (some (odd) name) S 1200 ..."): the field after the last ")".
func ParseProcStatPPID(stat []byte) (int, error) {
	s := string(stat)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0, fmt.Errorf("proc stat: unexpected %q", s)
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, fmt.Errorf("proc stat: unexpected %q", s)
	}
	return strconv.Atoi(f[1])
}
