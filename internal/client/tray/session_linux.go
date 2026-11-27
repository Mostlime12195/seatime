package tray

import (
	"fmt"
	"os"
	"time"

	"fyne.io/systray"
	"github.com/godbus/dbus/v5"
)

// sessionOK reports whether there is a session bus to offer the icon on.
// Without one (a server, an ssh login) there is nowhere to show it. It
// looks before it connects, because the bus library would otherwise start
// a bus of its own. The icon library then uses this same connection.
func sessionOK() bool {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		if _, err := os.Stat(fmt.Sprintf("/run/user/%d/bus", os.Getuid())); err != nil {
			return false
		}
	}
	_, err := dbus.SessionBus()
	return err == nil
}

// name gives the item its name in the desktop's list of status items.
func name() { systray.SetTitle("SeaTime") }

// again sets the look once more a moment later. The library announces
// itself on the bus while the menu is being built, and what is set in the
// instant between its two steps never reaches the bus: the item would
// stay blank.
func again(look func()) {
	time.AfterFunc(1500*time.Millisecond, look)
	time.AfterFunc(15*time.Second, look)
}
