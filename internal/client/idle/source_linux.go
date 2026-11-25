package idle

import (
	"context"
	"os"
	"os/exec"
	"time"
)

func defaultSource() Source {
	srcs, reason := LinuxSources(os.Getenv, func(bin string) bool { _, err := exec.LookPath(bin); return err == nil })
	if len(srcs) == 0 {
		return None{Reason: reason}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return First(ctx, srcs...)
}
