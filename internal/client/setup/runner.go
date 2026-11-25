package setup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"seatime/internal/client/config"
)

// Runner is how setup touches the outside world (systemctl, crontab,
// launchctl, schtasks, starting/stopping the daemon). It is injectable so the
// service fallback logic is testable without running any of them.
type Runner interface {
	LookPath(name string) (string, error)
	// Run executes a command and returns combined output.
	Run(stdin string, name string, args ...string) (string, error)
	// Start launches a detached long-lived process that outlives us.
	Start(exe string, args ...string) error
	// Kill terminates a process by pid.
	Kill(pid int) error
}

type realRunner struct{}

func (realRunner) LookPath(n string) (string, error) { return exec.LookPath(n) }
func (realRunner) Run(stdin, name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	var b bytes.Buffer
	c.Stdout, c.Stderr = &b, &b
	err := c.Run()
	return strings.TrimSpace(b.String()), err
}
func (realRunner) Start(exe string, args ...string) error {
	_ = os.MkdirAll(config.DataDir(), 0o700)
	logf, _ := os.OpenFile(filepath.Join(config.DataDir(), "daemon.err"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	c := exec.Command(exe, args...)
	c.Stdout, c.Stderr = logf, logf
	detach(c)
	if err := c.Start(); err != nil {
		return err
	}
	_ = c.Process.Release()
	if logf != nil {
		logf.Close()
	}
	return nil
}

// StartDetached launches exe as an independent background process (no
// console window on Windows), with its output going to daemon.err.
func StartDetached(exe string, args ...string) error { return realRunner{}.Start(exe, args...) }

func (realRunner) Kill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return terminate(p)
}

// dryRunner pretends everything works (used for --dry-run).
type dryRunner struct{}

func (dryRunner) LookPath(n string) (string, error)            { return n, nil }
func (dryRunner) Run(_, _ string, _ ...string) (string, error) { return "", nil }
func (dryRunner) Start(string, ...string) error                { return nil }
func (dryRunner) Kill(int) error                               { return nil }

// fakeRunner is used when SEATIME_FAKE_HOME is set (tests and sandboxes): nothing
// real runs. Knobs, all env vars:
//
//	SEATIME_FAKE_MISSING=crontab,xdg-open  LookPath fails for these
//	SEATIME_FAKE_FAIL=systemctl            Run fails for these (systemctl fails
//	                                    with the real "user scope bus" message)
//	SEATIME_FAKE_SPAWN=1                   really start/kill the daemon (it is
//	                                    sandboxed by SEATIME_FAKE_HOME/SEATIME_HOME)
//
// `crontab` is emulated against $SEATIME_FAKE_HOME/.fake-crontab.
type fakeRunner struct{}

func listHas(env, name string) bool {
	for _, p := range strings.Split(os.Getenv(env), ",") {
		if strings.TrimSpace(p) == name {
			return true
		}
	}
	return false
}

func (fakeRunner) LookPath(n string) (string, error) {
	if listHas("SEATIME_FAKE_MISSING", n) {
		return "", exec.ErrNotFound
	}
	return "/fake/" + n, nil
}

func (fakeRunner) Run(stdin, name string, args ...string) (string, error) {
	if listHas("SEATIME_FAKE_FAIL", name) {
		if name == "systemctl" {
			return "Failed to connect to user scope bus via local transport: No medium found", errors.New("exit status 1")
		}
		return "simulated failure", errors.New("exit status 1")
	}
	if name == "crontab" {
		f := filepath.Join(config.Home(), ".fake-crontab")
		if len(args) == 1 && args[0] == "-l" {
			b, err := os.ReadFile(f)
			if err != nil {
				return "no crontab for fake", errors.New("exit status 1")
			}
			return string(b), nil
		}
		if len(args) == 1 && args[0] == "-r" {
			_ = os.Remove(f)
			return "", nil
		}
		if len(args) == 1 && args[0] == "-" {
			return "", os.WriteFile(f, []byte(stdin), 0o600)
		}
	}
	return "", nil
}

func (fakeRunner) Start(exe string, args ...string) error {
	if os.Getenv("SEATIME_FAKE_SPAWN") == "1" {
		return realRunner{}.Start(exe, args...)
	}
	return nil
}

func (fakeRunner) Kill(pid int) error {
	if os.Getenv("SEATIME_FAKE_SPAWN") == "1" {
		return realRunner{}.Kill(pid)
	}
	return nil
}

func (o Options) runner() Runner {
	switch {
	case o.Runner != nil:
		return o.Runner
	case o.DryRun:
		return dryRunner{}
	case os.Getenv("SEATIME_FAKE_HOME") != "":
		return fakeRunner{}
	}
	return realRunner{}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func errText(out string, err error) string {
	if out = firstLine(out); out != "" {
		return fmt.Sprintf("%s (%v)", out, err)
	}
	return err.Error()
}
