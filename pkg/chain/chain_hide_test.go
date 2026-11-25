package chain

import (
	"testing"
	"time"

	"seatime/pkg/event"
)

func TestHidePreservesLeaf(t *testing.T) {
	e := event.Event{Seq: 1, TS: time.Unix(1700000000, 0), Kind: event.Prompt, Text: "make the tests pass", Command: "go test ./..."}
	a, _ := Leaf(e)
	b, _ := Leaf(Hide(e))
	if a != b {
		t.Fatalf("hide changed leaf")
	}
	e2 := e
	e2.Text = "something else"
	c, _ := Leaf(e2)
	if c == a {
		t.Fatalf("text not bound")
	}
}
