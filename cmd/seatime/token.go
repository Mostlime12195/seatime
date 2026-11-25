package main

import (
	"fmt"

	"seatime/internal/client/localauth"
)

// cmdToken prints the per-install secret that scripts send to the daemon's
// /seatime/emit endpoint; "rotate" replaces it.
func cmdToken(args []string) error {
	if len(args) > 0 {
		if args[0] != "rotate" {
			return fmt.Errorf("usage: seatime token [rotate]")
		}
		if err := localauth.Rotate(); err != nil {
			return err
		}
	}
	t, err := localauth.Secret()
	if err != nil {
		return err
	}
	fmt.Println(t)
	return nil
}
