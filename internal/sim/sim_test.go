package sim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandbox is Sandbox with t.Setenv so the environment is restored.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sb := filepath.Join(dir, "sandbox")
	_ = os.MkdirAll(sb, 0o700)
	for k, v := range map[string]string{
		"SEATIME_FAKE_HOME": sb, "HOME": sb, "SEATIME_HOME": filepath.Join(sb, "seatime"), "XDG_DATA_HOME": filepath.Join(sb, "data"),
		"XDG_CONFIG_HOME": filepath.Join(sb, "config"),
		"SEATIME_SERVER":  "", "SEATIME_IGNORE": "", "CLAUDE_CODE_ENTRYPOINT": "", "CLAUDE_CODE_SESSION_ATTENDED": "",
	} {
		t.Setenv(k, v)
	}
	return dir
}

// TestRedTeamGate is the fast regression subset of seatimesim: the honest
// personas must land in their fair range, verified, and the key
// attacks must keep their verdicts. Run everything with
// `go run ./cmd/seatimesim -details`.
func TestRedTeamGate(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end simulation")
	}
	dir := sandbox(t)
	got := map[string]*Result{}
	var ps []*Persona
	for _, p := range append(HonestPersonas(), Attacks()...) {
		if p.Gate {
			ps = append(ps, p)
		}
	}
	for _, p := range ps {
		r := RunPersona(p, dir, 42)
		got[p.Name] = r
		if r.Err != nil {
			t.Errorf("%s: %v", p.Name, r.Err)
			continue
		}
		if p.Honest && !r.Pass {
			t.Errorf("honest %s: credited %.2fh, fair %.2f-%.2fh, flags %v", p.Name, r.O.Credited, r.Truth[0], r.Truth[1], r.O.Flags)
		}
		if p.Want != "" && r.Verdict != p.Want {
			t.Errorf("attack %s: verdict %q, want %q (credited %.2fh for %.2fh effort, flags %v)", p.Name, r.Verdict, p.Want, r.O.Credited, r.O.Work, r.O.FlagCodes())
		}
	}
	if t.Failed() {
		t.Log("\n" + FormatTable(sortedResults(got)))
	}

	// no pay-to-win: the free model's human gets at least 0.8x the opus user's ratio
	if f, o := got["free_model_retries"], RunPersona(byName(HonestPersonas(), "opus_user"), dir, 42); f != nil && o != nil && f.O != nil && o.O != nil {
		if rf, ro := f.O.Credited/f.O.Work, o.O.Credited/o.O.Work; rf < 0.8*ro {
			t.Errorf("pay-to-win: free model ratio %.2f < 0.8 x opus %.2f", rf, ro)
		}
	}
	// faking costs about as much as doing, for attacks the fixes closed
	for name, maxMult := range map[string]float64{
		// backfill_synced_device: the last 15 min (AnchorGrace) before the
		// 17:00 checkpoint look live, and the idle timer does not remove them
		"backfill_synced_device": 0.35, "backfill_offline": 0.2, "two_device_double_count": 1.0,
		// rubber_stamp_hours: the rule credits a person who reads each turn
		// and replies after 20 s at the flat weight
		"split_projects": 1.0, "rubber_stamp_hours": 0.8,
	} {
		if r := got[name]; r != nil && r.O != nil && r.Multiplier > maxMult {
			t.Errorf("%s: multiplier %.2f > %.2f", name, r.Multiplier, maxMult)
		}
	}
	// the bucket path can't be backfilled and can't hold more than one person-minute per minute
	if r := got["forged_sync_buckets"]; r != nil && r.O != nil && r.O.Credited > 2.2 {
		t.Errorf("forged buckets: %.1fh visible on the compat API after 2h of live posting", r.O.Credited)
	}
}

func sortedResults(m map[string]*Result) []*Result {
	var out []*Result
	for _, k := range sortedKeys(m) {
		out = append(out, m[k])
	}
	return out
}

// TestSandboxed makes sure the simulator never points at a real home.
func TestSandboxed(t *testing.T) {
	sandbox(t)
	if h := os.Getenv("HOME"); !strings.Contains(h, os.TempDir()) && !strings.HasPrefix(h, "/tmp") {
		t.Fatalf("HOME=%s", h)
	}
}

func byName(ps []*Persona, name string) *Persona {
	for _, p := range ps {
		if p.Name == name {
			return p
		}
	}
	return nil
}
