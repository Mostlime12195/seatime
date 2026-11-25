package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"seatime/internal/client/emit"
	"seatime/internal/client/stats"
	"seatime/internal/client/store"
)

// cmdStatusline prints one short line for a harness status bar: today's
// counted time on the current project and in all. Claude Code calls it with
// its status JSON on stdin ({"workspace":{"current_dir":...}}); any other
// caller can pass nothing. It never fails loudly: on any problem it prints
// nothing, so a status bar is never broken.
//
// Claude Code: in ~/.claude/settings.json add
//
//	"statusLine": {"type": "command", "command": "seatime statusline"}
func cmdStatusline(args []string) error {
	defer func() { recover() }()
	in := readStdin(os.Stdin, 300*time.Millisecond)
	var p struct {
		CWD       string `json:"cwd"`
		Workspace struct {
			CurrentDir string `json:"current_dir"`
		} `json:"workspace"`
	}
	_ = json.Unmarshal(in, &p)
	dir := p.Workspace.CurrentDir
	if dir == "" {
		dir = p.CWD
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	st, err := openStore()
	if err != nil {
		return nil
	}
	defer st.Close()
	writeStatusline(os.Stdout, st, dir, time.Now())
	return nil
}

func writeStatusline(w io.Writer, st *store.Store, dir string, now time.Time) {
	day := stats.StartOfDay(now, now.Location())
	res, _, err := stats.Credit(st, day, now.Add(time.Second))
	if err != nil {
		return
	}
	key := ""
	if dir != "" {
		key = emit.New(st, "").Res.ForDir(dir).Key // the folder's project, whatever its remote is now
	}
	if sec, ok := res.ByProject[key]; ok && key != "" {
		fmt.Fprintf(w, "seatime %s on %s · %s today", stats.Dur(sec), stats.ProjectName(st.Label(key)), stats.Dur(res.Total))
		return
	}
	fmt.Fprintf(w, "seatime %s today", stats.Dur(res.Total))
}
