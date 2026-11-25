package main

import (
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/store"
)

// Kill -9 hooks mid-write, 100 times, several at once: the log always
// reopens, the chain verifies, and every hook that finished is stored.
func TestKillNineHooksMidWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns and kills 100 processes")
	}
	home := hookEnv(t)
	t.Setenv("SEATIME_FAKE_HOME", home)
	cwd := filepath.Join(home, "code", "app")
	os.MkdirAll(cwd, 0o755)
	payload := `{"session_id":"s1","cwd":"` + cwd + `","hook_event_name":"UserPromptSubmit","prompt":"add retries to the uploader"}`
	rng := rand.New(rand.NewSource(1))
	var mu sync.Mutex
	finished := 0
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		delay := time.Duration(rng.Intn(12000)) * time.Microsecond
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := seatimeCmd(t, payload, "hook", "claude-code", "UserPromptSubmit")
			if err := cmd.Start(); err != nil {
				t.Error(err)
				return
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err == nil {
					mu.Lock()
					finished++
					mu.Unlock()
				}
			case <-time.After(delay):
				cmd.Process.Signal(syscall.SIGKILL)
				<-done
			}
		}()
		if i%4 == 3 {
			wg.Wait()
		}
	}
	wg.Wait()
	st, rec, err := store.OpenChecked(config.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if rec.Quarantined != "" {
		t.Fatalf("a killed hook damaged the log: %s", rec.Reason)
	}
	n, err := st.VerifyChain()
	if err != nil {
		t.Fatalf("chain broken after kills: %v", err)
	}
	if n < int64(2*finished) {
		t.Fatalf("%d hooks finished but only %d events stored", finished, n)
	}
	if n%2 != 0 {
		// a hook writes prompt then agent_start; a kill between the two is fine,
		// but each stored event must be whole (VerifyChain checked the JSON)
		t.Logf("one hook was killed between its two events")
	}
	t.Logf("%d of 100 hooks finished, %d killed; %d events, chain intact", finished, 100-finished, n)
}
