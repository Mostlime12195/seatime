package promptcheck

import (
	"time"

	"seatime/pkg/event"
)

// A prompt that was not typed. A device counts the keyboard and mouse
// input before each prompt (event.Event.InputMS). A prompt with more words
// than a person can type in that input was pasted, dictated or sent by a
// program. This is the one rule about it: the reviewer sees the mark, and
// where the server says so (Set.Plain) such a prompt counts for no longer
// than the shortest kind of prompt does, whatever its verdict. How
// fast a person can type is the server's setting (Set.TypedWPM); a device
// has no number for it and decides nothing.

// NotTyped reports whether prompt e has more words than wpm words a minute
// allow for the input before it. Time the device could not watch counts as
// input. A prompt whose input was not measured (an older device, a
// terminal over SSH, a desktop with no idle timer) is never one, and
// neither is any prompt with wpm 0.
func NotTyped(e event.Event, wpm float64) bool {
	if e.Kind != event.Prompt || e.InputMS == nil || wpm <= 0 {
		return false
	}
	had := time.Duration(*e.InputMS+max(e.InputUnwatchedMS, 0)) * time.Millisecond
	return float64(e.Words) > wpm*had.Minutes()
}

// typed is how long prompt e counts when its verdict gives it m.
func (s *Set) typed(e event.Event, m time.Duration) time.Duration {
	if s.Plain > 0 && NotTyped(e, s.TypedWPM) {
		return min(m, Mins(s.Plain))
	}
	return m
}
