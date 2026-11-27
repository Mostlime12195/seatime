package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"seatime/internal/client/config"
	"seatime/internal/client/daemon"
	"seatime/internal/client/setup"
)

func probeDaemon(cfg config.Config) setup.Probe {
	m, ok := daemon.Ping(cfg)
	if !ok {
		return setup.Probe{}
	}
	p := setup.Probe{Alive: true}
	if f, ok := m["pid"].(float64); ok {
		p.PID = int(f)
	}
	p.Version, _ = m["version"].(string)
	return p
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cmdSetup(args []string, uninstall bool) error {
	fs := newFlagSet("setup")
	dry := fs.Bool("dry-run", false, "show what would change without changing anything")
	yes := fs.Bool("yes", false, "do everything with no prompts: hooks, service, WakaTime/Hackatime proxy if you have a plugin, then log in")
	fs.BoolVar(yes, "y", false, "same as --yes")
	server := fs.String("server", "", "seatime server URL (saved to config)")
	harness := fs.String("harness", "", "only this harness (comma-separated); setup re-enables it, uninstall opts it out of auto-hooking")
	waka := fs.Bool("wakatime-proxy", false, "point ~/.wakatime.cfg at seatime's local endpoint (original is saved; heartbeats are still forwarded there unchanged)")
	noWaka := fs.Bool("no-wakatime", false, "never touch ~/.wakatime.cfg, even with --yes")
	also := fs.Bool("also-hackatime", true, "forward editor heartbeats unchanged to your previous WakaTime/Hackatime api_url (default; --also-hackatime=false stops it, saved)")
	noSvc := fs.Bool("no-service", false, "don't install the background service")
	noTray := fs.Bool("no-tray", false, "no menu bar icon (saved; `seatime tray on` brings it back)")
	noCopy := fs.Bool("no-copy", false, "don't copy the binary into the data dir")
	noLogin := fs.Bool("no-login", false, "with --yes: skip the device login")
	noBrowser := fs.Bool("no-browser", false, "with --yes: don't open a browser for login")
	verbose := fs.Bool("verbose", false, "echo the system commands setup runs")
	purge := fs.Bool("purge", false, "uninstall: also delete all local data")
	fs.Parse(args)
	o := setup.Options{DryRun: *dry, WakaProxy: *waka, NoService: *noSvc, NoTray: *noTray, NoCopy: *noCopy,
		Out: os.Stdout, Yes: *yes, NoWaka: *noWaka, Verbose: *verbose, Harness: splitList(*harness),
		Server: *server, Version: version, Probe: probeDaemon}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "also-hackatime" {
			o.AlsoHackatime = also // only an explicit flag changes the saved choice
		}
	})
	if uninstall {
		return setup.Uninstall(o, *purge)
	}
	fmt.Println("seatime setup")
	rep, err := setup.Setup(o)
	if err != nil {
		return err
	}
	if o.DryRun {
		return nil
	}
	login := ""
	if *yes && !*noLogin {
		login = loginStep(rep.Server, *noBrowser)
	} else if _, ok := config.LoadAuth(); !ok {
		login = "not linked yet: run `seatime login`"
	}
	rep.Summary(os.Stdout, login)
	return nil
}

// loginStep links this device unless it already is linked to server.
func loginStep(server string, noBrowser bool) string {
	if a, ok := config.LoadAuth(); ok && strings.TrimRight(a.Server, "/") == strings.TrimRight(server, "/") {
		return "already logged in as " + a.Username
	}
	user, err := doLogin(server, noBrowser)
	if err != nil {
		return fmt.Sprintf("not linked yet (%v); run `seatime login` when you're ready", err)
	}
	return "logged in as " + user
}
