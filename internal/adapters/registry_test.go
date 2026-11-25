package adapters

import "testing"

func TestRegistry(t *testing.T) {
	want := []string{"claude-code", "codex", "cursor", "gemini-cli", "opencode",
		"copilot", "windsurf", "antigravity", "kilo", "aider", "cline", "roo-code", "kilo-code", "amp"}
	all := All()
	if len(all) != len(want) {
		t.Fatalf("got %d adapters", len(all))
	}
	for i, n := range want {
		if all[i].Name() != n || ByName(n) == nil {
			t.Errorf("adapter %d = %q want %q", i, all[i].Name(), n)
		}
	}
	if ByName("nope") != nil {
		t.Fatal("unexpected adapter")
	}
}
