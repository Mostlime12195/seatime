package presence

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"seatime/internal/client/idle"
)

// procTerminal finds the terminal of this process or its nearest ancestor
// (the AI tool's own terminal: the hook's stdin is a pipe) through /proc and
// reads its access time, which the kernel moves when keys arrive.
type procTerminal struct{}

func terminal() Terminal { return procTerminal{} }

func (procTerminal) Idle(context.Context) (time.Duration, error) {
	pid := os.Getpid()
	for i := 0; i < 6 && pid > 1; i++ {
		for _, fd := range []string{"0", "1", "2"} {
			dev, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/fd/" + fd)
			if err != nil || !(strings.HasPrefix(dev, "/dev/pts/") || strings.HasPrefix(dev, "/dev/tty")) {
				continue
			}
			var st syscall.Stat_t
			if err := syscall.Stat(dev, &st); err != nil {
				continue
			}
			return time.Since(time.Unix(st.Atim.Sec, st.Atim.Nsec)), nil
		}
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			break
		}
		if pid, err = ParseProcStatPPID(b); err != nil {
			break
		}
	}
	return 0, fmt.Errorf("no terminal: %w", ErrUnsupported)
}

// x11Windows asks the X server for the active window's process. On Wayland
// it is not used: most compositors do not say which window is in front.
type x11Windows struct{ run run }

func windows(getenv func(string) string) Windows {
	if idle.Wayland(getenv) || getenv("DISPLAY") == "" {
		return nil
	}
	return x11Windows{run: execRun}
}

func (x x11Windows) FrontPID(ctx context.Context) (int, error) {
	out, err := x.run(ctx, "xprop", "-root", "_NET_ACTIVE_WINDOW")
	if err != nil {
		return 0, fmt.Errorf("xprop: %w", err)
	}
	id, err := ParseXpropActive(out)
	if err != nil {
		return 0, err
	}
	if out, err = x.run(ctx, "xprop", "-id", id, "_NET_WM_PID"); err != nil {
		return 0, fmt.Errorf("xprop: %w", err)
	}
	return ParseXpropPID(out)
}

func (x11Windows) Parent(pid int) (int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	return ParseProcStatPPID(b)
}
