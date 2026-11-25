// Package logx is a tiny append-only file logger for hooks and the daemon.
// It never fails loudly: logging problems are swallowed.
package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"seatime/internal/client/config"
)

const maxSize = 1 << 20

func Printf(format string, args ...any) {
	p := config.LogPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if st, err := os.Stat(p); err == nil && st.Size() > maxSize {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02T15:04:05.000Z07:00"), fmt.Sprintf(format, args...))
}
