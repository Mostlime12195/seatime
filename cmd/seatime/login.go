package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/syncer"
)

func cmdLogin(args []string) error {
	fs := newFlagSet("login")
	server := fs.String("server", "", "server URL (default "+config.DefaultServer+" or $SEATIME_SERVER)")
	noBrowser := fs.Bool("no-browser", false, "don't try to open a browser")
	fs.Parse(args)
	cfg := config.Load()
	if *server != "" {
		cfg.Server = *server
		if err := config.Save(cfg); err != nil {
			return err
		}
	}
	_, err := doLogin(cfg.ServerURL(), *noBrowser)
	return err
}

// doLogin runs the device-code flow against base: prints the code, opens the
// browser when it can, waits for approval and saves the credentials.
func doLogin(base string, noBrowser bool) (string, error) {
	priv, pub, err := syncer.NewKey()
	if err != nil {
		return "", err
	}
	c := syncer.NewClient(base, "", version)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt) // ctrl-c skips login, not the whole setup
	defer stop()
	start, err := c.DeviceStart(ctx, syncer.DeviceName(), pub)
	if err != nil {
		return "", fmt.Errorf("can't reach %s: %w", base, err)
	}
	fmt.Printf("\nYour login code:  %s\n", start.UserCode)
	if !noBrowser && openBrowser(start.VerificationURL) {
		fmt.Printf("Opened %s in your browser; approve this device there.\n", start.VerificationURL)
	} else {
		fmt.Printf("Open this link (on any device) and approve:\n  %s\n", start.VerificationURL)
	}
	fmt.Println("Waiting for approval... (ctrl-c to skip; run `seatime login` any time)")
	interval := time.Duration(start.Interval) * time.Second
	if interval < time.Second {
		interval = 3 * time.Second
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	if start.ExpiresIn == 0 {
		deadline = time.Now().Add(10 * time.Minute)
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", errors.New("skipped")
		case <-time.After(interval):
		}
		r, err := c.DevicePoll(ctx, start.DeviceCode)
		if errors.Is(err, syncer.ErrPending) {
			continue
		}
		if err != nil {
			var he *syncer.HTTPError
			if errors.As(err, &he) && he.Status >= 500 {
				continue
			}
			return "", err
		}
		a := config.Auth{Token: r.Token, DeviceID: r.DeviceID, Username: r.Username, Server: base, PrivateKey: priv, PublicKey: pub}
		if err := config.SaveAuth(a); err != nil {
			return "", err
		}
		fmt.Printf("Logged in as %s (device %s). Credentials saved to %s (private to you).\n", r.Username, r.DeviceID, config.AuthPath())
		return r.Username, nil
	}
	return "", errors.New("login timed out; run `seatime login` again")
}
