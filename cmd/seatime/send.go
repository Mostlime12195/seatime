package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/daemon"
	"seatime/internal/client/records"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
)

// cmdSend syncs and sends this computer's records now. SeaTime does both
// on its own every minute or two while you work; this is for when someone
// does not want to wait for that.
func cmdSend(args []string) error {
	st, err := store.Open(config.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	notLinked := errors.New("this computer is not linked to an account: run `seatime login`")
	if _, err := (&syncer.Syncer{Store: st, Version: version}).SyncOnce(ctx); errors.Is(err, syncer.ErrNotLoggedIn) {
		return notLinked
	} else if err != nil {
		return err
	}
	d := &daemon.Daemon{Version: version, Store: st}
	n, err := d.RecordsOnce(ctx)
	switch {
	case errors.Is(err, records.ErrUnsupported) && !records.Sent(st):
		// a server that does not take records: the stretches of work, as before
		if n, err = d.SegmentsNow(ctx, time.Now()); err != nil {
			return err
		}
		fmt.Printf("sent %d stretch(es) of work; the server has checked them\n", n)
		return nil
	case errors.Is(err, syncer.ErrNotLoggedIn):
		return notLinked
	case err != nil:
		return err
	}
	if n == 0 {
		fmt.Println("the server already has every record")
		return nil
	}
	fmt.Printf("sent %d record(s). The server counts them within the hour.\n", n)
	return nil
}
