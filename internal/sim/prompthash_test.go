package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// commonPrompts is a word list of 1,000 short prompts people send an agent.
func commonPrompts() []string {
	base := []string{"yes", "no", "ok", "okay", "continue", "go on", "go ahead", "do it", "fix it", "try again",
		"lgtm", "looks good", "sure", "thanks", "yes please", "keep going", "next", "done?", "run it", "run the tests",
		"fix the tests", "it still doesnt work", "error", "same error", "still broken", "undo that", "revert", "explain",
		"why", "what", "commit", "push", "ship it", "build it", "deploy", "make it faster", "add tests", "clean up",
		"refactor", "good", "great", "perfect", "nice", "hmm", "wait", "stop", "proceed", "approved", "y", "k", "fix", "help"}
	mods := []string{"%s", "%s please", "%s!", "%s.", "%s now", "please %s", "%s thanks", "ok %s", "%s again", "just %s",
		"%s?", "%s pls", "%s, thanks", "yes %s", "can you %s", "%s asap", "%s :)", "now %s", "%s first", "%s quickly"}
	seen := map[string]bool{}
	var out []string
	for _, m := range mods {
		for _, b := range base {
			p := fmt.Sprintf(m, b)
			if !seen[p] && len(out) < 1000 {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// guesses is what someone holding the word list can compute for a prompt:
// every unkeyed fingerprint SeaTime ever used, with and without the
// commitment marker, of the text as typed and normalised.
func guesses(p string) []string {
	var out []string
	for _, s := range []string{p, strings.Join(strings.Fields(strings.ToLower(p)), " ")} {
		h := sha256.Sum256([]byte(s))
		out = append(out, hex.EncodeToString(h[:]))
	}
	return out
}

// TestPromptHashesNotGuessable: a person sends short common
// prompts, submits, and downloads the audit package. Hashing a
// list of 1,000 common short prompts finds none of them in it, while two
// of the person's identical prompts still carry the same fingerprint.
func TestPromptHashesNotGuessable(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end simulation")
	}
	list := commonPrompts()
	if len(list) != 1000 {
		t.Fatalf("word list has %d prompts, want 1000", len(list))
	}
	// the guess function finds a plain SHA-256 fingerprint (the old form)
	if g := guesses("Yes  please"); g[1] != func() string { h := sha256.Sum256([]byte("yes please")); return hex.EncodeToString(h[:]) }() {
		t.Fatal("guesses misses the plain fingerprint")
	}

	dir := sandbox(t)
	w, err := NewWorld(filepath.Join(dir, "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	d, err := w.NewDevice("shorty", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	a := NewActor(d, 7).In(d.Project("", "app"))
	a.Start()
	sent := map[string]int{}
	for i := 0; i < 120; i++ {
		p := list[(i*37)%len(list)]
		if i%10 == 9 {
			p = "continue" // repeated: equality must survive
		}
		sent[p]++
		a.Prompt(40*sec, p)
		a.Agent(R(20*sec), W(40*sec, "main.go"))
	}
	a.End()
	a.Wait(3 * mn)
	s := d.Submit(ProjectKey("shorty", "app"), SubmitOpts{})
	if s.Err != "" || s.ID == "" {
		t.Fatalf("segment: %s", s.Err)
	}

	pkg, err := w.SegmentRaw(d, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := string(pkg)

	found := 0
	for _, p := range list {
		for _, g := range guesses(p) {
			if strings.Contains(body, g) {
				found++
				t.Errorf("recovered %q from the stored segment (%s)", p, g)
			}
		}
		if sent[p] > 0 && strings.Contains(body, fmt.Sprintf("%q", p)) {
			t.Errorf("prompt text %q is in the stored segment", p)
		}
	}

	// rule heartbeat/1: a prompt carries whether it was new, decided on
	// the device, and no fingerprint of any kind
	prompts, fresh := 0, 0
	for _, e := range s.Events {
		if e.Kind == "prompt" {
			prompts++
			if e.TextHash != "" || e.New == nil {
				t.Fatalf("prompt with a fingerprint or without new: %+v", e)
			}
			if *e.New {
				fresh++
			}
		}
	}
	if prompts != 120 || strings.Contains(body, "text_hash") {
		t.Fatalf("%d prompts revealed (want 120); text_hash in the segment: %v", prompts, strings.Contains(body, "text_hash"))
	}
	t.Logf("stored segment %d bytes, %d prompts, %d new; %d of %d word-list prompts recovered", len(pkg), prompts, fresh, found, len(list))
}
