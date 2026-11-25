package main

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"seatime/internal/client/segment"
	"seatime/internal/client/syncer"
	"seatime/pkg/event"
)

// cmdName sets the name a project shows under on the server. Until one is
// chosen the server shows the folder's name (or a public repo's own name):
// the name only, never the folder's path.
func cmdName(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: seatime name <project> <name to show>   (or --clear)")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	key, err := segment.ResolveProject(st, args[0])
	if err != nil {
		return err
	}
	name := strings.TrimSpace(strings.Join(args[1:], " "))
	switch {
	case name == "":
		n, _ := st.GetMeta(syncer.NameKey(key))
		if n == "" {
			fmt.Printf("%s shows as %q on the server (no name chosen)\n", key, shownName(key))
		} else {
			fmt.Printf("%s shows as %q on the server\n", key, n)
		}
		return nil
	case name == "--clear":
		name = ""
	case utf8.RuneCountInString(name) > 80:
		return fmt.Errorf("a project name is at most 80 characters")
	}
	if err := st.SetMeta(syncer.NameKey(key), name); err != nil {
		return err
	}
	if name == "" {
		// the server takes a folder's name only for a project nobody named
		fmt.Printf("%s has no chosen name on this computer now. The server keeps the name it shows until you choose another here or on the Settings page.\n", key)
	} else {
		fmt.Printf("%s will show as %q from the next sync\n", key, name)
	}
	return nil
}

// shownName is what the server shows for a project with no chosen name:
// the folder's name, or a public repo's own.
func shownName(key string) string {
	if n := event.FolderName(key); n != "" {
		return n
	}
	return key[strings.LastIndexAny(key, "/:")+1:]
}
