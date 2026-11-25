// Command seatimesim runs seatime's end-to-end simulator: honest personas and the
// red-team attacks, each driven through the real client pipeline against an
// in-process seatimed under a fake clock.
//
//	seatimesim                       # everything, markdown tables on stdout
//	seatimesim -run beginner,replay  # personas whose name contains any of these
//	seatimesim -details              # per-persona submissions, flags and notes
//	seatimesim -seed 7               # other human timings
//	seatimesim -demo-db /path/seatime.db # every persona as its own user in ONE seatimed database (public showcase)
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"seatime/internal/sim"
)

func main() {
	run := flag.String("run", "", "comma-separated substrings of persona names to run (default all)")
	seed := flag.Uint64("seed", 42, "RNG seed for human timing")
	details := flag.Bool("details", false, "print per-persona details")
	dir := flag.String("dir", "", "scratch directory (default: $SEATIME_SIM_DIR or a temp dir)")
	honestOnly := flag.Bool("honest", false, "honest personas only")
	attacksOnly := flag.Bool("attacks", false, "attacks only")
	demoDB := flag.String("demo-db", "", "write ONE seatimed SQLite database holding every selected persona as its own demo-* user (serve it with SEATIME_DB), then exit")
	flag.Parse()

	base := *dir
	if base == "" {
		base = os.Getenv("SEATIME_SIM_DIR")
	}
	if base == "" {
		var err error
		if base, err = os.MkdirTemp("", "seatimesim-"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer os.RemoveAll(base)
	}
	// Repositories and editor files live under base; a relative path would
	// leave the client unable to match a file to its repository.
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	sim.Sandbox(filepath.Join(base, "sandbox"))

	if *demoDB != "" {
		out, err := filepath.Abs(*demoDB)
		if err == nil {
			err = buildDemo(out, base, *seed, *run)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	var ps []*sim.Persona
	if !*attacksOnly {
		ps = append(ps, sim.HonestPersonas()...)
	}
	if !*honestOnly {
		ps = append(ps, sim.Attacks()...)
	}
	var rs []*sim.Result
	for _, p := range ps {
		if *run != "" && !matches(p.Name, *run) {
			continue
		}
		r := sim.RunPersona(p, base, *seed)
		fmt.Fprintf(os.Stderr, "%-34s %6.1fs", p.Name, r.Elapsed.Seconds())
		if r.Err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR %v", r.Err)
		}
		fmt.Fprintln(os.Stderr)
		rs = append(rs, r)
	}
	fmt.Println(sim.FormatTable(rs))
	if *details {
		fmt.Println(sim.Details(rs))
	}
}

func buildDemo(out, base string, seed uint64, run string) error {
	users, err := sim.BuildDemo(sim.DemoOptions{Out: out, Dir: filepath.Join(base, "demo"), Seed: seed, Run: run,
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }})
	if err != nil {
		return err
	}
	list := filepath.Join(filepath.Dir(out), "demo-users.tsv")
	var b strings.Builder
	b.WriteString("username\tkind\tpersona\tverdict\tsubmission ids\n")
	for _, u := range users {
		kind := "attack"
		if u.Honest {
			kind = "honest"
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\n", u.Username, kind, u.Persona, u.Verdict, strings.Join(u.Submissions, ","))
	}
	if err := os.WriteFile(list, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("%d demo users in %s (list: %s)\nserve it: SEATIME_DB=%s SEATIME_SECRET=$(openssl rand -hex 32) seatimed\n", len(users), out, list, out)
	return nil
}

func matches(name, pats string) bool {
	for _, p := range strings.Split(pats, ",") {
		if p = strings.TrimSpace(p); p != "" && strings.Contains(name, p) {
			return true
		}
	}
	return false
}
