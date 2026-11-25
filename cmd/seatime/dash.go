package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"

	"seatime/internal/client/config"
	"seatime/internal/client/daemon"
	"seatime/internal/client/dash"
	"seatime/internal/client/stats"
)

func cmdDash(args []string) error {
	fs := newFlagSet("dash")
	port := fs.Int("port", config.Load().DashPortOrDefault(), "port on 127.0.0.1")
	open := fs.Bool("open", true, "open a browser")
	fs.Parse(args)
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", *port)
	fmt.Println("seatime dashboard:", url, "(ctrl-c to stop)")
	if *open {
		openBrowser(url)
	}
	cfg := config.Load()
	d := dash.New(st, *port)
	d.GoalHours = cfg.WeeklyGoalHours
	d.Counting = func() stats.Counting {
		ping, alive := daemon.Ping(cfg)
		return countingNow(st, cfg, ping, alive)
	}
	return d.Serve(ln)
}

// openBrowser tries to open url in the user's browser; false means it could
// not (headless box, no opener), so the caller should show the URL prominently.
func openBrowser(url string) bool {
	if os.Getenv("SEATIME_FAKE_HOME") != "" {
		return false
	}
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return false // headless / ssh session
		}
		if _, err := exec.LookPath("xdg-open"); err != nil {
			return false
		}
		c = exec.Command("xdg-open", url)
	}
	if c.Start() != nil {
		return false
	}
	go func() { _ = c.Wait() }()
	return true
}
