package presence

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// psTerminal asks ps which terminal this process's nearest ancestor with
// one uses, and reads that device's access time.
type psTerminal struct{ run run }

func terminal() Terminal { return psTerminal{run: execRun} }

func (t psTerminal) Idle(ctx context.Context) (time.Duration, error) {
	pid := os.Getpid()
	for i := 0; i < 6 && pid > 1; i++ {
		out, err := t.run(ctx, "/bin/ps", "-o", "tty=,ppid=", "-p", strconv.Itoa(pid))
		if err != nil {
			break
		}
		f := strings.Fields(string(out))
		if len(f) == 2 && f[0] != "??" && f[0] != "-" {
			var st syscall.Stat_t
			if err := syscall.Stat("/dev/"+f[0], &st); err == nil {
				return time.Since(time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)), nil
			}
		}
		if len(f) < 2 {
			break
		}
		if pid, err = strconv.Atoi(f[len(f)-1]); err != nil {
			break
		}
	}
	return 0, fmt.Errorf("no terminal: %w", ErrUnsupported)
}

// macWindows asks Launch Services for the front app's process (lsappinfo;
// believed to need no permission prompt, unverified until a real Mac run).
type macWindows struct{ run run }

func windows(func(string) string) Windows { return macWindows{run: execRun} }

func (m macWindows) FrontPID(ctx context.Context) (int, error) {
	asn, err := m.run(ctx, "/usr/bin/lsappinfo", "front")
	if err != nil {
		return 0, fmt.Errorf("lsappinfo: %w", err)
	}
	out, err := m.run(ctx, "/usr/bin/lsappinfo", "info", "-only", "pid", strings.TrimSpace(string(asn)))
	if err != nil {
		return 0, fmt.Errorf("lsappinfo: %w", err)
	}
	return ParseLsappinfoPID(out)
}

func (m macWindows) Parent(pid int) (int, error) {
	out, err := m.run(context.Background(), "/bin/ps", "-o", "ppid=", "-p", strconv.Itoa(pid))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}
