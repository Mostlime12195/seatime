//go:build windows || linux || (darwin && cgo)

package tray

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"fyne.io/systray"
)

// Supported reports whether this build can show an icon.
const Supported = true

// ReleaseMainThread undoes what the icon library does when the program
// starts: it ties the main goroutine to the first thread, which only the
// icon needs. Every other command calls this first and runs as before.
func ReleaseMainThread() { runtime.UnlockOSThread() }

const (
	refreshEvery = 30 * time.Second
	flashFor     = 6 * time.Second
	startingFor  = 20 * time.Second
)

// row is one menu item; it remembers what it shows, so a refresh touches
// only what changed.
type row struct {
	it      *systray.MenuItem
	title   string
	shown   bool
	enabled bool
}

func newRow(title string) *row {
	return &row{it: systray.AddMenuItem(title, ""), title: title, shown: true, enabled: true}
}

// newInfo is a row that only says something: not shown until it has text.
func newInfo() *row {
	r := newRow(" ")
	r.it.Disable()
	r.it.Hide()
	r.shown, r.enabled = false, false
	return r
}

// set shows the row with title (hides it when title is "").
func (r *row) set(title string, enabled bool) {
	if title == "" {
		if r.shown {
			r.it.Hide()
			r.shown = false
		}
		return
	}
	if title != r.title {
		r.it.SetTitle(title)
		r.title = title
	}
	if enabled != r.enabled {
		if enabled {
			r.it.Enable()
		} else {
			r.it.Disable()
		}
		r.enabled = enabled
	}
	if !r.shown {
		r.it.Show()
		r.shown = true
	}
}

type ui struct {
	o Options
	c client

	today, unchecked, now, problem *row
	signIn, open, send, update     *row
	version, hide                  *row

	st        *Status
	first     string // the daemon's version when the icon first heard from it
	last      time.Time
	started   time.Time
	downSince time.Time       // since when the daemon has not answered
	hold      map[*row]string // a row's text while its click is being carried out or answered
	quit      bool
	restart   bool // quit to start again on a new version
}

// Run shows the icon until it is hidden, turned off or replaced by a new
// version (ErrRestart). It returns at once when another icon is running.
func Run(o Options) error {
	if !sessionOK() {
		return ErrUnsupported
	}
	u := &ui{o: o, c: client{o.Port}, hold: map[*row]string{}}
	if o.Check {
		systray.Run(u.check, nil)
		fmt.Println("tray ok")
		return nil
	}
	release, ok := acquire(10 * time.Second)
	if !ok {
		return nil
	}
	defer release()
	Unhide()
	systray.Run(u.ready, nil)
	if u.restart {
		return ErrRestart
	}
	return nil
}

// look sets the icon and its name.
func look() {
	if iconIsTemplate {
		systray.SetTemplateIcon(icon, icon)
	} else {
		systray.SetIcon(icon)
	}
	systray.SetTooltip("SeaTime")
	name()
}

func (u *ui) build() {
	look()
	again(look)
	u.today, u.unchecked, u.now, u.problem = newInfo(), newInfo(), newInfo(), newInfo()
	u.signIn = newRow("Sign in")
	systray.AddSeparator()
	u.open, u.send, u.update = newRow("Open SeaTime"), newRow("Send now"), newRow("Check for updates")
	systray.AddSeparator()
	u.version, u.hide = newInfo(), newRow("Hide until next login")
	u.show(Menu(nil, u.o.Version).starting())
}

// check is Options.Check: the menu with sample numbers, then quit.
func (u *ui) check() {
	u.build()
	u.show(Menu(&Status{Version: u.o.Version, Account: "sample", Site: "https://example.com", Now: "sample",
		Today: &Totals{Seconds: 8100, UncheckedSeconds: 720, UncheckedPrompts: 3}, Updates: true}, u.o.Version))
	go func() {
		time.Sleep(2 * time.Second)
		systray.Quit()
	}()
}

// ready builds the menu and returns: the library serves the menu only once
// this has returned. The loop carries on by itself.
func (u *ui) ready() {
	u.build()
	go u.loop()
}

func (u *ui) loop() {
	u.started = time.Now()
	u.refresh()
	tick := time.NewTimer(u.wait())
	defer tick.Stop()
	type answer struct {
		r    *row
		text string
	}
	answers := make(chan answer, 2)
	unhold := make(chan *row, 2)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, stopSignals...)
	click := func(r *row, busy, path string) {
		if u.hold[r] != "" {
			return
		}
		u.hold[r] = busy
		u.show(u.view())
		go func() {
			rep, ok := u.c.act(path)
			answers <- answer{r, Flash(rep, ok, u.o.Version)}
		}()
	}
	for !u.quit {
		select {
		case <-stop: // uninstall, tray off, logout
			u.quit = true
		case <-tick.C:
			u.refresh()
			tick.Reset(u.wait())
		case <-systray.TrayOpenedCh:
			if time.Since(u.last) > 2*time.Second {
				u.refresh()
			}
		case a := <-answers:
			u.hold[a.r] = a.text
			u.show(u.view())
			time.AfterFunc(flashFor, func() { unhold <- a.r })
		case r := <-unhold:
			delete(u.hold, r)
			u.refresh()
		case <-u.open.it.ClickedCh:
			if u.st != nil {
				u.openURL(u.st.Site)
			}
		case <-u.signIn.it.ClickedCh:
			if u.st == nil || !u.openURL(u.st.SignIn) {
				if u.o.SignIn != nil {
					u.o.SignIn()
				}
			}
		case <-u.send.it.ClickedCh:
			click(u.send, "Sending…", "send")
		case <-u.update.it.ClickedCh:
			click(u.update, "Checking…", "update")
		case <-u.hide.it.ClickedCh:
			Hide()
			u.quit = true
		}
	}
	systray.Quit()
}

// refresh asks the daemon and redraws. It also notices that the icon was
// turned off, and that the daemon now runs a newer version than this one:
// the icon then quits, to start again on that version.
func (u *ui) refresh() {
	u.st, u.last = u.c.status(), time.Now()
	if st := u.st; st != nil {
		switch {
		case st.Off:
			u.quit = true
		case u.first == "":
			u.first = st.Version
		case st.Version != u.first && st.Version != u.o.Version:
			u.quit, u.restart = true, true
		}
	}
	u.show(u.view())
}

// view is the menu for the last status. While the daemon has not answered
// yet (both start at login), it says so instead of calling it stopped.
func (u *ui) view() View {
	v := Menu(u.st, u.o.Version)
	if u.st == nil && u.first == "" && time.Since(u.started) < startingFor {
		return v.starting()
	}
	return v
}

// wait is the time to the next refresh: short while the daemon does not
// answer, so the menu fills in as soon as it is up.
func (u *ui) wait() time.Duration {
	if u.st != nil {
		u.downSince = time.Time{}
		return refreshEvery
	}
	if u.downSince.IsZero() {
		u.downSince = time.Now()
	}
	if time.Since(u.downSince) < 2*time.Minute {
		return 3 * time.Second
	}
	return refreshEvery
}

// openURL opens a web page in the browser; false when there is none to open.
func (u *ui) openURL(url string) bool {
	if u.o.OpenURL == nil || !(strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")) {
		return false
	}
	u.o.OpenURL(url)
	return true
}

func (u *ui) show(v View) {
	u.today.set(v.Today, false)
	u.unchecked.set(v.Unchecked, false)
	u.now.set(v.Now, false)
	u.problem.set(v.Problem, false)
	u.signIn.set(when(v.SignIn, "Sign in"), true)
	u.open.set("Open SeaTime", v.Open)
	u.held(u.send, "Send now", v.Send)
	u.held(u.update, when(v.Update, "Check for updates"), true)
	u.version.set(v.Version, false)
}

// held shows a clickable row, or the text of its click in progress.
func (u *ui) held(r *row, title string, enabled bool) {
	if h := u.hold[r]; h != "" {
		r.set(h, false)
		return
	}
	r.set(title, enabled)
}

func when(ok bool, s string) string {
	if ok {
		return s
	}
	return ""
}
