package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"seatime/internal/client/config"
	"seatime/internal/client/segment"
	"seatime/internal/client/syncer"
	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/ledger"
)

// cmdInspect prints exactly what would leave this machine: the next sync's
// request bodies (or, with --segment, a project's records as a segment), byte for byte,
// with one line per field explaining it from the wire allowlist (WIRE.md).
func cmdInspect(args []string) error {
	fs := newFlagSet("inspect")
	sub := fs.String("segment", "", "show this project's records in the form the daemon sends them in, instead of the next sync")
	onlyJSON := fs.Bool("json", false, "print only the exact JSON bodies")
	onlyFields := fs.Bool("fields", false, "print only the field list, not the JSON")
	beats := fs.String("heartbeats", "", "print the heartbeats a segment of this project carries, under Hackatime's field names")
	fs.Parse(args)
	if *beats != "" {
		*sub = *beats
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	auth, loggedIn := config.LoadAuth()
	if !loggedIn {
		// nothing is ever sent without a login; build with a throwaway key
		// so a new user can see what would leave once they log in
		priv, pub, _ := syncer.NewKey()
		auth = config.Auth{Token: "-", DeviceID: "(not logged in)", PrivateKey: priv, PublicKey: pub}
	}
	var bodies []any
	section, what := "sync", ""
	if *sub != "" {
		section = "segment"
		key, err := segment.ResolveProject(st, *sub)
		if err != nil {
			return err
		}
		head, _, _ := st.Head() // the daemon goes up to the synced head
		prep, err := segment.Build(st, auth, segment.Options{Project: key, MaxSeq: head})
		if err != nil {
			return err
		}
		if *beats != "" {
			return writeHeartbeats(os.Stdout, prep.Bundle)
		}
		bodies = append(bodies, prep.Bundle)
		what = fmt.Sprintf("All of %s's records in the form the daemon sends them in, after each sync, to %s/api/v1/records (shown here as one segment of that project)", *sub, auth.APIBase(config.Load()))
	} else {
		plan, ok, err := (&syncer.Syncer{Store: st, Version: version, Auth: &auth}).Next()
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Nothing new to send: the server already has everything up to this moment.")
			return nil
		}
		for _, r := range plan.Requests {
			bodies = append(bodies, r)
		}
		what = fmt.Sprintf("The next sync would send %d request(s) to %s/api/v1/sync", len(plan.Requests), plan.Server)
	}
	return writeInspect(os.Stdout, section, what, bodies, loggedIn, *onlyJSON, *onlyFields)
}

func writeInspect(w io.Writer, section, what string, bodies []any, loggedIn, onlyJSON, onlyFields bool) error {
	var raws [][]byte
	total := 0
	for _, b := range bodies {
		raw, err := json.Marshal(b) // the same encoding the client sends
		if err != nil {
			return err
		}
		raws = append(raws, raw)
		total += len(raw)
	}
	if onlyJSON {
		for _, r := range raws {
			fmt.Fprintf(w, "%s\n", r)
		}
		return nil
	}
	fmt.Fprintf(w, "%s, %d bytes in all.\n", what, total)
	if !loggedIn {
		fmt.Fprintln(w, "You are not logged in, so nothing is sent; this is what would be.")
	}
	fmt.Fprintln(w, "\nEvery field, how many values it carries, and what it is (from pkg/api/WIRE.md):")
	notes := api.WireNotes()
	counts := map[string]int{}
	for _, r := range raws {
		var v any
		_ = json.Unmarshal(r, &v)
		walkJSON(v, "", counts)
	}
	var paths []string
	for p := range counts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		note := notes[section+"."+p]
		if note == "" {
			note = "NOT ON THE ALLOWLIST (please report this)"
		}
		fmt.Fprintf(w, "  %-34s x%-6d %s\n", p, counts[p], note)
	}
	if onlyFields {
		return nil
	}
	for i, r := range raws {
		fmt.Fprintf(w, "\nRequest %d of %d, exactly as sent (%d bytes):\n%s\n", i+1, len(raws), len(r), r)
	}
	return nil
}

// writeHeartbeats prints, one JSON object a line, the heartbeats the rule
// counts from a submission's records, under Hackatime's field names
// (ledger.HackatimeHeartbeat): what Hackatime would receive, plus "new".
func writeHeartbeats(w io.Writer, b api.Bundle) error {
	for _, h := range ledger.Heartbeats(audit.Revealed(b)) {
		raw, err := json.Marshal(h.Hackatime(b.Project))
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\n", raw)
	}
	return nil
}

// walkJSON counts the values at each field path ("buckets[].project").
func walkJSON(v any, prefix string, counts map[string]int) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			if _, isObj := val.(map[string]any); !isObj {
				if _, isArr := val.([]any); !isArr {
					counts[p]++
				}
			}
			walkJSON(val, p, counts)
		}
	case []any:
		if len(x) == 0 {
			counts[prefix]++
		}
		for _, it := range x {
			if _, isObj := it.(map[string]any); isObj {
				walkJSON(it, prefix+"[]", counts)
			} else {
				counts[prefix]++ // a list of plain values: listed under its own name
			}
		}
	}
}
