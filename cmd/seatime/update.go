package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"seatime/internal/adapters"
	"seatime/internal/client/config"
	"seatime/internal/client/daemon"
	"seatime/internal/client/localauth"
	"seatime/internal/client/store"
	"seatime/internal/client/update"
	"seatime/pkg/event"
)

const updateUsage = `usage: seatime update [--check | off | on]

  seatime update          install the newest release now
  seatime update --check  only say whether there is a newer release
  seatime update off      stop updating automatically (the daemon checks once a day)
  seatime update on       update automatically again (the default)
`

// selfPath is this binary's real path (symlinks resolved).
func selfPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe
}

func newUpdater(exe string, notify bool) *update.Updater {
	return &update.Updater{Version: version, Exe: exe, BaseURL: config.Load().UpdateBase(),
		Dir: config.DataDir(), Notify: notify}
}

func cmdUpdate(args []string) error {
	cfg := config.Load()
	if len(args) == 1 && (args[0] == "on" || args[0] == "off") {
		on := args[0] == "on"
		cfg.AutoUpdate = &on
		if err := config.Save(cfg); err != nil {
			return err
		}
		if on {
			fmt.Println("Automatic updates are on: the daemon checks once a day.")
		} else {
			fmt.Println("Automatic updates are off. `seatime update` still updates when you ask.")
		}
		return nil
	}
	fs := newFlagSet("update")
	fs.Usage = func() { fmt.Fprint(os.Stderr, updateUsage) }
	check := fs.Bool("check", false, "only say whether there is a newer release")
	fs.Parse(args)
	if fs.NArg() > 0 {
		fs.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	u := newUpdater(selfPath(), false)
	if !update.IsRelease(version) {
		fmt.Printf("This is a development build (%s), which never updates itself. To switch to a release, use the install line on your SeaTime server's home page.\n", version)
		return nil
	}
	if *check {
		m, err := u.Latest(ctx)
		if err != nil {
			return err
		}
		switch {
		case update.Newer(m.Version, version):
			fmt.Printf("SeaTime %s is out (you have %s). Run `seatime update` to install it.\n", m.Version, version)
		default:
			fmt.Printf("SeaTime %s is the newest release.\n", version)
		}
		return nil
	}

	fmt.Printf("Checking %s ...\n", u.BaseURL)
	res, err := u.Apply(ctx)
	if err != nil {
		return err
	}
	switch {
	case res.Updated:
		fmt.Printf("Updated SeaTime %s -> %s (the old binary is kept at %s).\n", res.Current, res.Latest, u.Exe+".prev")
		fmt.Println(restartRunningDaemon(cfg, res.Latest))
	case res.RolledBack:
		return fmt.Errorf("%s failed its self-test on this computer (%s), so SeaTime stayed on %s", res.Latest, res.Detail, res.Current)
	case res.Skipped:
		fmt.Printf("%s failed its self-test here before, so SeaTime stays on %s until a newer release is out.\n", res.Latest, res.Current)
	default:
		fmt.Printf("SeaTime %s is the newest release.\n", version)
	}
	return nil
}

// restartRunningDaemon asks a running daemon to start again on the new binary
// and waits for it to come back. It returns one line for the user.
func restartRunningDaemon(cfg config.Config, want string) string {
	if _, alive := daemon.Ping(cfg); !alive {
		return "The background daemon is not running; it will use the new version when it starts."
	}
	tok, err := localauth.Secret()
	if err == nil {
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/seatime/restart", cfg.WakaPortOrDefault()), nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		var resp *http.Response
		if resp, err = (&http.Client{Timeout: 3 * time.Second}).Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				err = errors.New(resp.Status)
			}
		}
	}
	if err != nil {
		return "Could not restart the background daemon (" + err.Error() + "); run `seatime setup` to restart it on the new version."
	}
	var got any
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		if m, ok := daemon.Ping(cfg); ok {
			if got = m["version"]; got == want {
				return "The background daemon restarted on " + want + "."
			}
		}
	}
	if got != nil {
		return fmt.Sprintf("The background daemon restarted but runs %v (it was started from another copy of seatime).", got)
	}
	return "The background daemon is restarting."
}

// cmdSelfTest is `seatime doctor --self-test`: a fast, offline check that
// this binary works on this machine. The updater runs it on a freshly
// installed binary and puts the previous one back if it fails. It touches
// nothing of the user's: the database it writes is a throwaway.
func cmdSelfTest(w io.Writer) error {
	fmt.Fprintf(w, "seatime %s\n", version)
	if len(adapters.All()) == 0 {
		return errors.New("no harness adapters registered")
	}
	dir, err := os.MkdirTemp("", "seatime-selftest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(filepath.Join(dir, "selftest.db"))
	if err != nil {
		return fmt.Errorf("open a database: %w", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.Append(event.Event{Kind: event.SessionStart, Harness: "selftest", SessionID: "selftest"}); err != nil {
			st.Close()
			return fmt.Errorf("write an event: %w", err)
		}
	}
	n, err := st.VerifyChain()
	st.Close()
	if err != nil || n != 3 {
		return fmt.Errorf("hash chain: %d events, %v", n, err)
	}
	fmt.Fprintln(w, "database and hash chain ok")
	fmt.Fprintln(w, update.SelfTestLine(version))
	return nil
}

// updateCheck is the doctor line about updates.
func updateCheck() (level, detail string) {
	st := update.LoadState(config.DataDir())
	switch {
	case !update.IsRelease(version):
		return "ok", "development build (" + version + "): never updates itself"
	case !config.Load().AutoUpdateEnabled():
		return "warn", "automatic updates are off (`seatime update on`)"
	case st.LastError != "":
		return "warn", "last check " + ago(st.LastCheck) + " failed: " + st.LastError
	case st.LastCheck.IsZero():
		return "ok", "on; the daemon checks once a day"
	}
	return "ok", "on; last checked " + ago(st.LastCheck)
}

// showsNotice: commands a person runs, which may print a one-time update
// notice on stderr. Hooks, the daemon and scripted commands never do.
func showsNotice(cmd string, args []string) bool {
	switch cmd {
	case "status", "today", "projects", "dash", "version", "--version", "-v", "update":
		return true
	case "doctor":
		return !strings.Contains(strings.Join(args, " "), "--self-test")
	}
	return false
}
