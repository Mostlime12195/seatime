package server

import (
	"strings"
	"testing"
)

// /healthz is 200 while the database answers, and 503 once shutdown has
// begun or when the database is gone.
func TestHealthIsCheapAndDBAware(t *testing.T) {
	e := newEnv(t)
	if code, out := e.apiDo("GET", "/healthz", "", nil); code != 200 || !strings.Contains(string(out), `"db":"ok"`) {
		t.Fatalf("healthy: %d %s", code, out)
	}
	e.srv.Drain()
	if code, _ := e.apiDo("GET", "/healthz", "", nil); code != 503 {
		t.Fatalf("draining: %d", code)
	}
	e2 := newEnv(t)
	e2.st.Close()
	if code, out := e2.apiDo("GET", "/healthz", "", nil); code != 503 || !strings.Contains(string(out), "db unavailable") {
		t.Fatalf("database closed: %d %s", code, out)
	}
}
