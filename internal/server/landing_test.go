package server

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"seatime/internal/server/store"
	"seatime/internal/server/store/storetest"
)

// The landing page states the number of the rule in force: with the
// prompt check off, the agent's 10 minutes after a new prompt; with it
// on, the most a prompt can get, read from the check's rules, so a
// change to them shows without a release.
func TestLandingStatesRuleInForce(t *testing.T) {
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err := New(Config{Dev: true, Secret: "s", PromptCheckRules: testRulesJSON()}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	get := func() string {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		return w.Body.String()
	}
	ctx := context.Background()

	off := get()
	if !strings.Contains(off, "Each new prompt counts for up to 10 minutes.") || strings.Contains(off, "up to 15") {
		t.Fatalf("check off:\n%s", off)
	}

	if err := srv.setSetting(ctx, "prompt_check", ptr("on"), "boss"); err != nil {
		t.Fatal(err)
	}
	on := get()
	if !strings.Contains(on, "Each prompt that you send counts for up to 15 minutes, depending on how good it is.") ||
		strings.Contains(on, "Each new prompt counts for up to") {
		t.Fatalf("check on:\n%s", on)
	}

	rules := strings.Replace(srv.rulesDefault, `"minutes": 15`, `"minutes": 12.5`, 1)
	if err := srv.setSetting(ctx, "prompt_check_rules", &rules, "boss"); err != nil {
		t.Fatal(err)
	}
	if got := get(); !strings.Contains(got, "counts for up to 12.5 minutes, depending") {
		t.Fatalf("edited rules not shown:\n%s", got)
	}
}
