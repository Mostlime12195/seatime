package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/daemon"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
)

// cmdSend uploads this computer's records now. SeaTime does this on its own
// about 3 hours after a stretch of work; this is for when someone wants to
// see the work checked sooner.
func cmdSend(args []string) error {
	st, err := store.Open(config.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	n, err := (&daemon.Daemon{Version: version, Store: st}).SegmentsNow(ctx, time.Now())
	if errors.Is(err, syncer.ErrNotLoggedIn) {
		return errors.New("this computer is not linked to an account: run `seatime login`")
	}
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Println("nothing new to send (records go up after the next sync, which runs every minute)")
		return nil
	}
	fmt.Printf("sent %d stretch(es) of work; the server has checked them\n", n)
	return nil
}
