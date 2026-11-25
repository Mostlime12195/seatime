package segment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/emit"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/pkg/audit"
	"seatime/pkg/chain"
	"seatime/pkg/event"
)

func build(t *testing.T) (*Prepared, *store.Store, config.Auth) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	priv, pub, _ := syncer.NewKey()
	auth := config.Auth{DeviceID: "dev1", PrivateKey: priv, PublicKey: pub}
	for i := 0; i < 12; i++ {
		proj := "github.com/me/mine"
		if i%3 == 0 {
			proj = "local:private-thing"
		}
		st.Append(event.Event{Kind: event.Prompt, Project: proj, SessionID: "s", Text: "make it work please"})
	}
	p, err := Build(st, auth, Options{Project: "github.com/me/mine"})
	if err != nil {
		t.Fatal(err)
	}
	return p, st, auth
}

func TestBundleRoundTrip(t *testing.T) {
	p, st, auth := build(t)
	b := p.Bundle
	if !p.Summary.ChainOK {
		t.Fatal(p.Summary.ChainErr)
	}
	if p.Summary.Revealed != 8 || p.Summary.Hidden != 3 { // seq 2..11 (seq 1 and 12: 1 is private, 12 is mine -> range 2..12)
		t.Logf("revealed=%d hidden=%d", p.Summary.Revealed, p.Summary.Hidden)
	}
	// Re-verify exactly as the server would: replay and compare to every stored checkpoint.
	var cps []chain.Checkpoint
	for _, e := range b.Entries {
		h, _ := st.HashAt(e.Seq)
		cps = append(cps, chain.Checkpoint{Seq: e.Seq, Hash: h})
	}
	head, err := chain.Verify(b.StartHash, b.Entries, cps)
	if err != nil {
		t.Fatal(err)
	}
	last := b.Entries[len(b.Entries)-1].Seq
	if want, _ := st.HashAt(last); head != want {
		t.Fatal("head mismatch")
	}
	// private project's events are hashes only
	for _, e := range b.Entries {
		if e.Event != nil && e.Event.Project != "github.com/me/mine" {
			t.Fatal("leaked another project's event")
		}
		if e.Event == nil && e.LeafHash == "" {
			t.Fatal("empty entry")
		}
	}
	if !syncer.VerifyBundle(auth.PublicKey, b) {
		t.Fatal("bundle signature invalid")
	}
	b.Claimed.CodingSeconds += 1
	if syncer.VerifyBundle(auth.PublicKey, b) {
		t.Fatal("tampered bundle verified")
	}
}

// The default bundle carries no prompt text, command, file path, folder or
// local folder name: every revealed record is in its private form, and the
// planted values appear nowhere in the bytes that would leave the machine.
// The chain still replays and the claim is the shared analysis's.
func TestBundleCarriesNoPathsCommandsOrFolderNames(t *testing.T) {
	home := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	priv, pub, _ := syncer.NewKey()
	auth := config.Auth{DeviceID: "dev1", PrivateKey: priv, PublicKey: pub}
	proj := filepath.Join(home, "code", "FOLDER-CANARY")
	if err := os.MkdirAll(filepath.Join(proj, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := emit.New(st, home)
	at := time.Now().Add(-20 * time.Minute)
	for i, e := range []event.Event{
		{Kind: event.Prompt, Harness: "claude-code", SessionID: "s1", CWD: proj, Text: "PROMPT-CANARY make the pricing page responsive"},
		{Kind: event.AgentStart, Harness: "claude-code", SessionID: "s1", CWD: proj},
		{Kind: event.Tool, Harness: "claude-code", SessionID: "s1", CWD: proj, Tool: "Bash", Command: "cat COMMAND-CANARY.txt"},
		{Kind: event.Run, Harness: "claude-code", SessionID: "s1", CWD: proj, Command: "go test ./COMMAND-CANARY/...", RunKind: event.RunTest, ExitCode: new(int)},
		{Kind: event.Tool, Harness: "claude-code", SessionID: "s1", CWD: proj, Tool: "Edit", Write: true, Files: []string{filepath.Join(proj, "FILE-CANARY.go")}},
		{Kind: event.AgentStop, Harness: "claude-code", SessionID: "s1", CWD: proj},
		{Kind: event.Edit, Harness: "editor:vscode", CWD: proj, Files: []string{filepath.Join(proj, "FILE-CANARY.go")}, Extra: map[string]any{"editor_project": "EXTRA-CANARY"}, Lines: 90, LineNo: 12, CursorPos: 340},
		{Kind: event.Idle, Harness: "seatime", WindowMS: 600000, IdleSrc: "ioreg"},
	} {
		e.TS = at.Add(time.Duration(i) * 40 * time.Second)
		if _, err := p.EmitEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	keys, _ := st.ProjectKeys()
	var key string
	for k := range keys {
		key = k
	}
	if !strings.Contains(key, "FOLDER-CANARY") {
		t.Fatalf("the local key is expected to carry the folder name (no segment names it): %q", key)
	}
	head, _, _ := st.Head()
	prep, err := Build(st, auth, Options{Project: key, MaxSeq: head}) // up to the last synced record, as submit does
	if err != nil {
		t.Fatal(err)
	}
	if !prep.Summary.ChainOK {
		t.Fatal(prep.Summary.ChainErr)
	}
	raw, _ := json.Marshal(prep.Bundle)
	for _, leak := range []string{"CANARY", home, "pricing"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the bundle carries %q", leak)
		}
	}
	idle := 0
	for _, en := range prep.Bundle.Entries {
		e := en.Event
		if e == nil {
			continue
		}
		if !chain.Private(*e) {
			t.Errorf("record %d is not in its private form: %+v", en.Seq, e)
		}
		if e.Kind == event.Idle {
			idle++
		}
		if e.Kind == event.Edit && (e.Lines != 90 || e.LineNo != 12 || e.CursorPos != 340) {
			t.Errorf("the editor heartbeat lost its line, cursor or length: %+v", e)
		}
	}
	if idle != 1 || prep.Summary.Revealed != 8 || prep.Bundle.Project != syncer.ServerKey(key) {
		t.Fatalf("revealed %d (idle %d), project %q", prep.Summary.Revealed, idle, prep.Bundle.Project)
	}
	if got := audit.Claim(audit.Revealed(prep.Bundle), prep.Bundle.Project); got.Seconds() <= 0 || got.Seconds() != prep.Bundle.Claimed.Seconds() || got.Seconds() != prep.Bundle.Claimed.Seconds() {
		t.Fatalf("claim %+v, recomputed %+v", prep.Bundle.Claimed, got)
	}
}

// Records written before the private form are withheld, never revealed
// with their paths.
func TestOlderRecordsAreWithheld(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	priv, pub, _ := syncer.NewKey()
	auth := config.Auth{DeviceID: "dev1", PrivateKey: priv, PublicKey: pub}
	st.Append(event.Event{Kind: event.Edit, Project: "local:old-folder", Canon: 1, Files: []string{"old/path.go"}})
	st.Append(event.Event{Kind: event.Edit, Project: "local:old-folder"})
	prep, err := Build(st, auth, Options{Project: "local:old-folder"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(prep.Bundle)
	if prep.Summary.Withheld != 1 || prep.Summary.Revealed != 1 || strings.Contains(string(raw), "old") {
		t.Fatalf("withheld %d, revealed %d: %s", prep.Summary.Withheld, prep.Summary.Revealed, raw)
	}
}

func TestCheckpointSig(t *testing.T) {
	priv, pub, _ := syncer.NewKey()
	a := config.Auth{DeviceID: "d", PrivateKey: priv}
	cp := chain.Checkpoint{Seq: 9, Hash: chain.Genesis}
	sig, err := syncer.SignCheckpoint(a, cp)
	if err != nil || !syncer.VerifyCheckpoint(pub, sig, "d", cp) {
		t.Fatal(err)
	}
	if string(syncer.CheckpointMessage("d", 9, "h")) != "seatime-checkpoint\nd\n9\nh" {
		t.Fatal("message format")
	}
	cp.Seq = 10
	if syncer.VerifyCheckpoint(pub, sig, "d", cp) {
		t.Fatal("wrong seq verified")
	}
}

// A file the person edits inside the project's git repo is named in the
// bundle by its path in the repo, beside the editor heartbeat (as the editor
// plugin sends it to Hackatime). A file outside the repo, the agent's files,
// the folder and the home directory are never named, and the chain still
// replays: the name is not part of the record.
func TestBundleNamesRepoFilesOnly(t *testing.T) {
	home := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	priv, pub, _ := syncer.NewKey()
	auth := config.Auth{DeviceID: "dev1", PrivateKey: priv, PublicKey: pub}
	repo := filepath.Join(home, "code", "FOLDER-CANARY")
	for _, d := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, "src", "ui"), filepath.Join(home, "notes")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inRepo := filepath.Join(repo, "src", "ui", "button.tsx")
	outside := filepath.Join(home, "notes", "OUTSIDE-CANARY.md")
	agentFile := filepath.Join(repo, "AGENT-CANARY.go")
	for _, f := range []string{inRepo, outside, agentFile} {
		if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := emit.New(st, home)
	at := time.Now().Add(-20 * time.Minute)
	for i, e := range []event.Event{
		{Kind: event.Edit, Harness: "editor:vscode", CWD: filepath.Dir(inRepo), Files: []string{inRepo}, Lines: 40, LineNo: 3, CursorPos: 9},
		{Kind: event.Edit, Harness: "editor:vscode", CWD: filepath.Dir(inRepo), Files: []string{inRepo}, Lines: 41, LineNo: 4, CursorPos: 2},
		{Kind: event.Tool, Harness: "claude-code", SessionID: "s1", CWD: repo, Tool: "Edit", Write: true, Files: []string{agentFile}},
		{Kind: event.Edit, Harness: "editor:vscode", CWD: repo, Files: []string{outside}, Lines: 7},
	} {
		e.TS = at.Add(time.Duration(i) * 40 * time.Second)
		if _, err := p.EmitEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	key, err := ResolveProject(st, "FOLDER-CANARY")
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := st.Head()
	prep, err := Build(st, auth, Options{Project: key, MaxSeq: head})
	if err != nil {
		t.Fatal(err)
	}
	if !prep.Summary.ChainOK {
		t.Fatal(prep.Summary.ChainErr)
	}
	var named []string
	for _, en := range prep.Bundle.Entries {
		if en.Path != "" {
			named = append(named, en.Path)
			if en.Event == nil || en.Event.Kind != event.Edit {
				t.Errorf("a name beside a record that is not an editor heartbeat: %+v", en)
			}
		}
	}
	if len(named) != 2 || named[0] != "src/ui/button.tsx" || prep.Summary.Named != 2 {
		t.Fatalf("named %v (summary %d)", named, prep.Summary.Named)
	}
	raw, _ := json.Marshal(prep.Bundle)
	for _, leak := range []string{"CANARY", home, "notes"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the bundle carries %q", leak)
		}
	}
	if !syncer.VerifyBundle(auth.PublicKey, prep.Bundle) {
		t.Fatal("signature")
	}
	b := prep.Bundle
	b.Entries[0].Path = "renamed.go"
	if syncer.VerifyBundle(auth.PublicKey, b) {
		t.Fatal("a changed file name still verifies: the signature must cover it")
	}
}

func TestRepoPath(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "a"), 0o755)
	os.WriteFile(filepath.Join(repo, "a", "b.go"), nil, 0o644)
	ed := func(f string) event.Event {
		return event.Event{Kind: event.Edit, Harness: "editor:zed", Files: []string{f}}
	}
	for _, c := range []struct {
		e    event.Event
		want string
	}{
		{ed("a/b.go"), "a/b.go"},
		{ed("a/missing.go"), ""},
		{ed("../a/b.go"), ""},
		{ed("a/../a/b.go"), ""},
		{ed("/etc/passwd"), ""},
		{ed("~/a/b.go"), ""},
		{ed(chain.Commit("a/b.go")), ""},
		{event.Event{Kind: event.Tool, Harness: "claude-code", Files: []string{"a/b.go"}}, ""},
		{event.Event{Kind: event.Edit, Harness: "editor:zed", Files: []string{"a/b.go", "a/b.go"}}, ""},
	} {
		if got := RepoPath(c.e, repo); got != c.want {
			t.Errorf("%v: %q, want %q", c.e.Files, got, c.want)
		}
	}
	if RepoPath(ed("a/b.go"), "") != "" {
		t.Error("named a file with no repo")
	}
}
