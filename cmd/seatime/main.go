// Command SeaTime is the client: CLI, hooks, background daemon.
package main

import (
	"fmt"
	"os"
	"runtime"

	"seatime/internal/client/config"
	"seatime/internal/client/tray"
	"seatime/internal/client/update"
)

var version = "dev"

const usage = `seatime - count the hours you spend building with AI

usage: seatime <command> [flags]

  setup       detect harnesses and install hooks, background service (--dry-run, --wakatime-proxy)
  uninstall   remove everything setup added (--purge also deletes local data)
  login       link this device to your SeaTime account
  status      what SeaTime is doing right now
  today       today's hours, per project
  projects    projects SeaTime has seen
  name        the name a project shows under on the server: seatime name <project> <name>
  dash        local web dashboard on 127.0.0.1
  tray        the menu bar icon; SeaTime starts it by itself (off / on: remove it or bring it back)
  forget      delete prompt text kept on this machine (--project X, --from-now-on)
  statusline  one line for a harness status bar: today's time on this project
  send        sync and send your records to the server now, without waiting for the next round
  inspect     show exactly what the next sync (or --segment <project>) would send
  emit        plugin protocol: JSON event(s) on stdin
  token       print the local secret for scripts that POST to the daemon (rotate: new secret)
  hook        called by harness hooks: seatime hook <adapter> <event>
  daemon      background sync + watchers + WakaTime-compatible endpoint
  doctor      diagnose problems (--self-test: quick offline check of this binary)
  update      install the newest release now (off / on: automatic daily updates)
  version
`

func main() {
	config.ApplyFakeHome()
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(0)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd != "tray" {
		tray.ReleaseMainThread() // only the icon needs it
	}
	if cmd == "hook" { // never fails
		runHook(args)
		os.Exit(0)
	}
	if showsNotice(cmd, args) {
		if n := update.TakeNotice(config.DataDir()); n != "" {
			fmt.Fprintln(os.Stderr, n)
		}
	}
	var err error
	switch cmd {
	case "setup":
		err = cmdSetup(args, false)
	case "uninstall":
		err = cmdSetup(args, true)
	case "login":
		err = cmdLogin(args)
	case "status":
		err = cmdStatus(args)
	case "today":
		err = cmdToday(args)
	case "projects":
		err = cmdProjects(args)
	case "name":
		err = cmdName(args)
	case "token":
		err = cmdToken(args)
	case "emit":
		err = cmdEmit(args)
	case "daemon":
		err = cmdDaemon(args)
	case "submit", "verify":
		err = fmt.Errorf("there is nothing to submit any more: SeaTime sends its records as you work, and applications read your hours from the server")
	case "inspect":
		err = cmdInspect(args)
	case "forget":
		err = cmdForget(args)
	case "statusline":
		err = cmdStatusline(args)
	case "dash":
		err = cmdDash(args)
	case "tray":
		err = cmdTray(args)
	case "send":
		err = cmdSend(args)
	case "doctor":
		err = cmdDoctor(args)
	case "update":
		err = cmdUpdate(args)
	case "version", "--version", "-v":
		fmt.Printf("seatime %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "seatime: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "seatime %s: %v\n", cmd, err)
		os.Exit(1)
	}
}
