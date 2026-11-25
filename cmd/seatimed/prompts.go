package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"seatime/internal/server"
	"seatime/internal/server/store"
	"seatime/pkg/promptcheck"
)

// runPrompts is `seatimed prompts ...`: ask an account's devices for their
// saved prompts, and see how far that got. It opens SEATIME_DB itself, so
// it runs next to the server (e.g. in its container).
//
//	seatimed prompts pull -user NAME -project KEY     what shipping KEY needs
//	seatimed prompts pull -user NAME -from T -to T    a range (RFC 3339), -project optional
//	seatimed prompts status -user NAME
func runPrompts(args []string, out io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(out, "usage: seatimed prompts pull|status -user NAME [-project KEY] [-from T -to T] [-wait 336h]")
		return 2
	}
	fs := flag.NewFlagSet("prompts "+args[0], flag.ContinueOnError)
	fs.SetOutput(out)
	user := fs.String("user", "", "the account's username")
	project := fs.String("project", "", "a project's server key (as on its page)")
	from := fs.String("from", "", "pull prompts sent from this time (RFC 3339)")
	to := fs.String("to", "", "until this time (RFC 3339)")
	wait := fs.Duration("wait", server.PullWait, "how long devices have to send them")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *user == "" {
		fmt.Fprintln(out, "-user is required")
		return 2
	}
	st, err := store.Open(server.ConfigFromEnv().DB)
	if err != nil {
		fmt.Fprintln(out, "open db:", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()
	u, err := st.UserByName(ctx, *user)
	if err != nil {
		fmt.Fprintln(out, "no such user:", *user)
		return 1
	}
	now := time.Now().UTC()
	switch args[0] {
	case "pull":
		var pulls []store.PromptPull
		if *from != "" || *to != "" {
			f, err1 := time.Parse(time.RFC3339, *from)
			t, err2 := time.Parse(time.RFC3339, *to)
			if err1 != nil || err2 != nil || !t.After(f) {
				fmt.Fprintln(out, "-from and -to must both be RFC 3339 times, -to after -from")
				return 2
			}
			p := store.PromptPull{UserID: u.ID, Project: *project, From: f, To: t, Created: now, Deadline: now.Add(*wait), By: "cli"}
			if p.ID, err = st.AddPromptPull(ctx, p); err != nil {
				fmt.Fprintln(out, "pull:", err)
				return 1
			}
			pulls = append(pulls, p)
		} else {
			if *project == "" {
				fmt.Fprintln(out, "give -project, or -from and -to")
				return 2
			}
			if pulls, err = server.ShipPull(ctx, st, u.ID, *project, "cli", now, *wait); err != nil {
				fmt.Fprintln(out, "pull:", err)
				return 1
			}
		}
		for _, p := range pulls {
			fmt.Fprintf(out, "pull %d: %s\n", p.ID, describePull(ctx, st, p))
		}
		fmt.Fprintln(out, "The devices send these prompts for their check on their next sync.")
	case "status":
		ps, err := st.UserCheckPeriods(ctx, u.ID)
		if err != nil {
			fmt.Fprintln(out, err)
			return 1
		}
		fmt.Fprintf(out, "%s's own setting: %s\n", u.Username, onOff(promptcheck.UserOnAt(ps, now)))
		for _, p := range ps {
			fmt.Fprintf(out, "  from %s: %s\n", p.From.Format(time.RFC3339), onOff(p.On))
		}
		devs, err := st.ListDevices(ctx, u.ID)
		if err != nil {
			fmt.Fprintln(out, err)
			return 1
		}
		for _, d := range devs {
			seq, _, err := st.PromptChainHead(ctx, d.ID)
			if err != nil {
				fmt.Fprintln(out, err)
				return 1
			}
			n, settled, _ := st.ChainResolve(ctx, d.ID, time.UnixMilli(0), promptcheck.Forever)
			fmt.Fprintf(out, "device %s (%s): %d prompt hashes, %d of %d new prompts settled\n", d.ID, d.Name, seq, settled, n)
		}
		pulls, err := st.PromptPulls(ctx, u.ID, false)
		if err != nil {
			fmt.Fprintln(out, err)
			return 1
		}
		for _, p := range pulls {
			fmt.Fprintf(out, "pull %d: %s\n", p.ID, describePull(ctx, st, p))
		}
	default:
		fmt.Fprintln(out, "unknown command:", args[0])
		return 2
	}
	return 0
}

func describePull(ctx context.Context, st store.Store, p store.PromptPull) string {
	scope := "every project"
	if p.Project != "" {
		scope = p.Project
	}
	n, settled, _ := st.PullResolve(ctx, p)
	state := "open"
	if p.Closed {
		state = "closed"
	}
	return fmt.Sprintf("%s, %s to %s, %d of %d prompts settled, %s, deadline %s", scope,
		p.From.Format(time.RFC3339), p.To.Format(time.RFC3339), settled, n, state, p.Deadline.Format(time.RFC3339))
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
