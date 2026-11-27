package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"seatime/internal/client/config"
	"seatime/internal/client/setup"
	"seatime/internal/client/tray"
)

const trayUsage = `usage: seatime tray [on | off]

  seatime tray      show the menu bar icon (SeaTime starts it by itself at login)
  seatime tray off  remove the icon, on this computer, for good
  seatime tray on   bring it back
`

// cmdTray is the menu bar icon. Nobody has to run it: setup and the daemon
// start it. It draws what the daemon tells it and passes clicks on.
func cmdTray(args []string) error {
	if len(args) == 1 && (args[0] == "on" || args[0] == "off") {
		return trayOnOff(args[0] == "on")
	}
	fs := newFlagSet("tray")
	fs.Usage = func() { fmt.Fprint(os.Stderr, trayUsage) }
	check := fs.Bool("check", false, "show the icon for a moment and quit (a test that this build can draw one here)")
	fs.Parse(args)
	if fs.NArg() > 0 {
		fs.Usage()
		os.Exit(2)
	}
	exe := selfPath() // before an update renames the running file
	trimFile(filepath.Join(config.DataDir(), "tray.err"), 1<<20)
	err := tray.Run(tray.Options{Version: version, Port: config.Load().WakaPortOrDefault(), OpenURL: openBrowser, Check: *check,
		SignIn: func() { _ = setup.StartDetached(exe, "login") }})
	switch {
	case errors.Is(err, tray.ErrRestart):
		if runtime.GOOS == "darwin" && os.Getenv("XPC_SERVICE_NAME") == setup.TrayLabel {
			os.Exit(3) // launchd starts the job again, on the new binary
		}
		return setup.StartDetached(exe, "tray")
	case errors.Is(err, tray.ErrUnsupported):
		if !tray.Supported {
			fmt.Println("This build of SeaTime has no menu bar icon. Everything else works as usual.")
		} else {
			fmt.Println("There is no desktop session here to show an icon in.")
		}
		return nil
	}
	return err
}

func trayOnOff(on bool) error {
	cfg := config.Load()
	cfg.Tray = &on
	if err := config.Save(cfg); err != nil {
		return err
	}
	o := setup.Options{Out: os.Stdout}
	if !on {
		setup.RemoveTray(o)
		fmt.Println("The menu bar icon is off. SeaTime counts as before.")
		return nil
	}
	if !tray.Supported {
		fmt.Println("This build of SeaTime has no menu bar icon.")
		return nil
	}
	tray.Unhide()
	if _, err := setup.EnsureTray(o, selfPath(), true); err != nil {
		return err
	}
	fmt.Println("The menu bar icon is on.")
	return nil
}

// trayCheck is the doctor line about the icon.
func trayCheck(cfg config.Config) (level, detail string) {
	switch {
	case !tray.Supported:
		return "ok", "not in this build"
	case !cfg.TrayEnabled():
		return "ok", "off (`seatime tray on`)"
	case tray.Running():
		return "ok", "running"
	case tray.Hidden():
		return "ok", "hidden until the next login"
	case runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "":
		return "ok", "no desktop session here"
	}
	return "warn", "not running (SeaTime starts it within 10 minutes, or at the next login)"
}
