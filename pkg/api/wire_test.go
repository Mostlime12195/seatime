package api

import (
	"reflect"
	"sort"
	"testing"
)

// Every field a client can send must be on the allowlist with a privacy
// note, and the allowlist must not name fields that no longer exist.
func TestWireAllowlistIsComplete(t *testing.T) {
	notes := WireNotes()
	want := map[string]bool{}
	for sec, typ := range WireTypes {
		for _, p := range WirePaths(typ) {
			want[sec+"."+p] = true
			if notes[sec+"."+p] == "" {
				t.Errorf("%s.%s crosses the wire but is not in WIRE.md (add it with a privacy note)", sec, p)
			}
		}
	}
	var extra []string
	for k := range notes {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("WIRE.md lists fields that no longer exist: %v", extra)
	}
}

// The check itself must notice a new field.
func TestWireAllowlistCatchesNewField(t *testing.T) {
	type sneaky struct {
		SyncReq
		WindowTitle string `json:"window_title"`
	}
	notes := WireNotes()
	missing := 0
	for _, p := range WirePaths(reflect.TypeOf(sneaky{})) {
		if notes["sync."+p] == "" {
			missing++
		}
	}
	if missing == 0 {
		t.Fatal("a struct with an unlisted field passed the allowlist")
	}
}
