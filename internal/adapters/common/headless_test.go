package common

import (
	"strings"
	"testing"
)

func TestSubcommandMatcher(t *testing.T) {
	cases := []struct {
		m    HeadlessMatcher
		args string
		want bool
	}{
		{SubcommandMatcher("codex", "exec"), "codex|exec|fix the tests", true},
		{SubcommandMatcher("codex", "exec"), "/usr/bin/node|/usr/lib/node_modules/@openai/codex/bin/codex.js|exec|--json|do it", true},
		{SubcommandMatcher("codex", "exec"), "codex", false},
		{SubcommandMatcher("codex", "exec"), "codex|--model|o4|explain the exec bit", false},
		{SubcommandMatcher("gemini", "-p", "--prompt"), "gemini|-p|write tests", true},
		{SubcommandMatcher("gemini", "-p", "--prompt"), "gemini|--prompt=hi", true},
		{SubcommandMatcher("gemini", "-p", "--prompt"), "gemini|-i|hello", false},
		{SubcommandMatcher("cursor-agent", "-p", "--print"), "cursor-agent|--print|refactor", true},
		{SubcommandMatcher("opencode", "run"), "opencode|run|add a test", true},
		{SubcommandMatcher("opencode", "run"), "bash|-c|opencode run x", false}, // "bash -c <cmd>" passes the command as one argument
		{SubcommandMatcher("claude", "-p", "--print"), "claude|-p|hi", true},
		{SubcommandMatcher("claude", "-p", "--print"), "claude|--resume", false},
		{SubcommandMatcher("claude", "-p", "--print"), "vim|-p|a|b", false},
	}
	for _, c := range cases {
		if got := c.m(strings.Split(c.args, "|")); got != c.want {
			t.Errorf("%q: %v, want %v", c.args, got, c.want)
		}
	}
	// the test process's own ancestors (go test) are never a headless harness
	if Headless(SubcommandMatcher("definitely-not-a-harness", "x")) {
		t.Error("ancestor scan matched nothing-like")
	}
}
