package api

import (
	"reflect"
	"sort"
	"strings"
	"time"
)

// WirePaths lists every JSON field path a value of type t can carry, e.g.
// "buckets[].project". The wire allowlist (WIRE.md) must name each one.
// A field tagged wire:"old" is skipped: this version never sets it, and it
// crosses the wire only on a record an older version wrote, unchanged
// because the hash chain commits to it.
func WirePaths(t reflect.Type) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(t reflect.Type, prefix string, depth int)
	walk = func(t reflect.Type, prefix string, depth int) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if depth > 6 {
			return
		}
		switch {
		case t == reflect.TypeOf(time.Time{}):
			return
		case t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8:
			walk(t.Elem(), prefix+"[]", depth+1)
			return
		case t.Kind() == reflect.Map:
			walk(t.Elem(), prefix+"{}", depth+1)
			return
		case t.Kind() != reflect.Struct:
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || f.Tag.Get("wire") == "old" {
				continue // wire:"old": only on records an older version wrote (see event.Event)
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if f.Anonymous && name == "" { // encoding/json flattens embedded structs
				walk(f.Type, prefix, depth)
				continue
			}
			if name == "" {
				name = f.Name
			}
			p := name
			if prefix != "" {
				p = prefix + "." + name
			}
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
			walk(f.Type, p, depth+1)
		}
	}
	walk(t, "", 0)
	sort.Strings(out)
	return out
}
