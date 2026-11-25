package config

import "testing"

func TestFrontOn(t *testing.T) {
	for _, c := range []struct {
		cfg   Config
		check bool
		want  bool
	}{
		{Config{}, false, false},                      // today: off unless set
		{Config{}, true, true},                        // on by default while the prompt check is on
		{Config{FrontWindowOff: true}, true, false},   // the person can turn it off
		{Config{FrontWindowCheck: true}, false, true}, // or on
	} {
		if got := FrontOn(c.cfg, c.check); got != c.want {
			t.Errorf("%+v check=%v: %v", c.cfg, c.check, got)
		}
	}
}
