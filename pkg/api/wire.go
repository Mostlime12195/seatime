package api

import (
	_ "embed"
	"reflect"
	"strings"
)

// WireMD is the allowlist of every field that may leave the user's machine,
// with a plain note per field (WIRE.md). The privacy page and `seatime inspect`
// read it; wire_test.go keeps it complete.
//
//go:embed WIRE.md
var WireMD string

// WireNotes maps "<section>.<path>" (sections as in WireTypes) to the allowlist note.
func WireNotes() map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(WireMD, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "- `") {
			continue
		}
		rest := l[3:]
		i := strings.Index(rest, "`")
		if i < 0 {
			continue
		}
		out[rest[:i]] = strings.TrimSpace(strings.TrimPrefix(rest[i+1:], ":"))
	}
	// a segment's entries are the entries of the records (chain.Entry):
	// WIRE.md lists their fields once, under the records
	for k, v := range out {
		if rest, ok := strings.CutPrefix(k, "records.entries"); ok && out["segment.entries"+rest] == "" {
			out["segment.entries"+rest] = v
		}
	}
	return out
}

// WireTypes are the request bodies a client sends, by allowlist section.
var WireTypes = map[string]reflect.Type{
	"sync":    reflect.TypeOf(SyncReq{}),
	"records": reflect.TypeOf(RecordsReq{}),
	"segment": reflect.TypeOf(Bundle{}),
	"check":   reflect.TypeOf(PromptCheckReq{}),
	"login":   reflect.TypeOf(DeviceStartReq{}),
	"health":  reflect.TypeOf(CheckIn{}),
	"result":  reflect.TypeOf(RequestResult{}),
}
