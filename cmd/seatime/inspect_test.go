package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/segment"
	"seatime/internal/client/syncer"
	"seatime/pkg/api"
	"seatime/pkg/event"
)

// On a seeded log, inspect shows every field with a note, and nothing that
// identifies content: no prompt text, no file or folder path, no command.
// Both what the next sync sends and what a segment would send. The
// folder's name goes in a sync, once, as the project's name; its path
// never does, and a segment does not name the folder at all.
func TestInspectShowsNoContent(t *testing.T) {
	home := hookEnv(t)
	t.Setenv("SEATIME_FAKE_HOME", home)
	priv, pub, _ := syncer.NewKey()
	config.SaveAuth(config.Auth{Token: "t", DeviceID: "dev1", PrivateKey: priv, PublicKey: pub, Server: "https://seatime.example"})
	proj := filepath.Join(home, "code", "secret-startup")
	os.MkdirAll(proj, 0o755)
	st, p, err := openPipeline()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-10 * time.Minute)
	for i, e := range []event.Event{
		{Kind: event.Prompt, Harness: "claude-code", SessionID: "s1", CWD: proj, Text: "PROMPT-TEXT-SHOULD-STAY make the pricing page"},
		{Kind: event.AgentStart, Harness: "claude-code", SessionID: "s1", CWD: proj},
		{Kind: event.Tool, Harness: "claude-code", SessionID: "s1", CWD: proj, Tool: "Bash", Command: "cat COMMAND-SHOULD-STAY.txt"},
		{Kind: event.Tool, Harness: "claude-code", SessionID: "s1", CWD: proj, Tool: "Edit", Write: true, Files: []string{filepath.Join(proj, "FILE-SHOULD-STAY.go")}},
		{Kind: event.AgentStop, Harness: "claude-code", SessionID: "s1", CWD: proj},
		{Kind: event.Edit, Harness: "editor:vscode", CWD: proj, Files: []string{filepath.Join(proj, "FILE-SHOULD-STAY.go")}},
	} {
		e.TS = now.Add(time.Duration(i) * 30 * time.Second)
		if _, err := p.EmitEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	plan, ok, err := (&syncer.Syncer{Store: st, Version: "v"}).Next()
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	key, err := segment.ResolveProject(st, "secret-startup")
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := st.Head()
	prep, err := segment.Build(st, config.Auth{DeviceID: "dev1", PrivateKey: priv, PublicKey: pub}, segment.Options{Project: key, MaxSeq: head})
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	var bodies []any
	for _, r := range plan.Requests {
		bodies = append(bodies, r)
	}
	// the sync names the project by its folder's name, and says it is one
	var metas []api.ProjectMeta
	for _, r := range plan.Requests {
		metas = append(metas, r.Projects...)
	}
	if len(metas) != 1 || metas[0].Name != "secret-startup" || !metas[0].Folder || metas[0].Key != event.HashedKey("local:secret-startup") || metas[0].Remote != "" {
		t.Fatalf("project metas %+v", metas)
	}
	var b bytes.Buffer
	if err := writeInspect(&b, "sync", "next sync", bodies, true, false, false); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(b.String(), "secret-startup"); n != 1 {
		t.Errorf("the sync carries the folder's name %d times, want once (projects[].name):\n%s", n, b.String())
	}
	syncLen := b.Len()
	if err := writeInspect(&b, "segment", "segment", []any{prep.Bundle}, true, false, false); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if testing.Verbose() {
		t.Log("\n" + out[:min(len(out), 2500)])
	}
	if strings.Contains(out[syncLen:], "secret-startup") {
		t.Errorf("the segment carries the folder's name:\n%s", out[syncLen:])
	}
	for _, leak := range []string{"PROMPT-TEXT", "COMMAND-SHOULD", "FILE-SHOULD", home, "code/", "/secret-startup", "pricing"} {
		if strings.Contains(out, leak) {
			t.Errorf("inspect output carries %q:\n%s", leak, out)
		}
	}
	if strings.Contains(out, "NOT ON THE ALLOWLIST") {
		t.Errorf("a field without a privacy note:\n%s", out)
	}
	for _, want := range []string{"buckets[].project", "checkpoint.hash", "bytes in all", "exactly as sent", "entries[].event.kind"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	// rule heartbeat/1: no fingerprint of any kind leaves with a prompt
	for _, hash := range []string{"text_hash", "sim_prev", "novelty", `"chars"`} {
		if strings.Contains(out, hash) {
			t.Errorf("inspect output carries %s", hash)
		}
	}

	// the heartbeats a submission carries, under Hackatime's field names:
	// every key is one Hackatime's API accepts (HEARTBEAT_KEYS in
	// app/controllers/api/hackatime/v1/hackatime_controller.rb, copied
	// below), plus "new"
	hackatimeKeys := strings.Fields(`ai_input_tokens ai_line_changes ai_model ai_output_tokens ai_prompt_length ai_session
		ai_subscription_plan branch category created_at cursorpos editor
		entity human_line_changes is_write language line_additions line_deletions lineno
		lines machine operating_system project project_root_count time type user_agent plugin new`)
	b.Reset()
	if err := writeHeartbeats(&b, prep.Bundle); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("heartbeats: %q", b.String())
	}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		for k := range m {
			if !slices.Contains(hackatimeKeys, k) {
				t.Errorf("heartbeat key %q is not one Hackatime accepts: %s", k, l)
			}
		}
		if m["category"] != "coding" && m["category"] != "ai coding" {
			t.Errorf("category %v", m["category"])
		}
	}
}
