package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"seatime/internal/client/config"
	"seatime/internal/client/segment"
)

// cmdForget deletes prompt text and agent shell commands from the local log,
// keeping their fingerprints so the hash chain and the hours are unchanged.
func cmdForget(args []string) error {
	fs := newFlagSet("forget")
	proj := fs.String("project", "", "only this project (default: every project)")
	yes := fs.Bool("yes", false, "don't ask")
	stop := fs.Bool("from-now-on", false, "also stop storing prompt text from now on (keeps a similarity sketch only)")
	fs.Parse(args)
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	key := ""
	if *proj != "" {
		if key, err = segment.ResolveProject(st, *proj); err != nil {
			return err
		}
	}
	what := "every project"
	if key != "" {
		what = key
	}
	if !*yes {
		fmt.Printf("Delete the prompt text and agent commands stored on this machine for %s? Your hours stay the same, except that prompts still waiting for the prompt check, or asked for later, cannot be checked and will not count. [y/N] ", what)
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			fmt.Println("Nothing deleted.")
			return nil
		}
	}
	n, err := st.Forget(key)
	if err != nil {
		return err
	}
	fmt.Printf("Deleted the text of %d record(s) for %s; only their fingerprints remain, so the hash chain is unchanged and a prompt you send again still counts as a repeat.\n", n, what)
	if *stop {
		cfg := config.Load()
		f := false
		cfg.KeepPromptText = &f
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Println("From now on SeaTime keeps no prompt text, only a similarity sketch (undo: set \"keep_prompt_text\": true in config.json).")
	}
	return nil
}
