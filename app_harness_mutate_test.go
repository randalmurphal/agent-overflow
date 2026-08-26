package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness"
)

// TestMutatingHarnessRPCsSerializeOnOneLock proves each world-rewriting
// RPC actually takes Harness.mutate, by holding the lock and watching the
// call park.
//
// It is deliberately a lock test rather than a timing race: the damage a
// missing lock does (a reset's RemoveAll deleting the repo a concurrent
// seed just wrote) is probabilistic, and a test that has to lose a race to
// fail is a test that passes by luck.
func TestMutatingHarnessRPCsSerializeOnOneLock(t *testing.T) {
	calls := map[string]func(*Harness) error{
		"HarnessSeed (via seed)": func(h *Harness) error {
			// Fails validation — but only AFTER taking the lock, which is
			// the property under test.
			_, err := h.seed(HarnessSeedSpec{})
			return err
		},
		"HarnessReset": func(h *Harness) error { return h.HarnessReset() },
		"HarnessReplayBundle": func(h *Harness) error {
			_, err := h.HarnessReplayBundle("no-such-bundle", harness.ReplayOptions{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			h, _ := newHarnessTestApp(t)
			h.mutate.Lock()

			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = call(h)
			}()

			select {
			case <-done:
				h.mutate.Unlock()
				t.Fatalf("%s ran while another mutation held Harness.mutate", name)
			case <-time.After(150 * time.Millisecond):
			}

			h.mutate.Unlock()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s never resumed after the lock was released", name)
			}
		})
	}
}

// TestConcurrentResetsBothSucceed is the behavioural half: two resets
// arriving at once — the e2e shape, where a suite-level reset and a
// test-level one can overlap — must both come back clean.
//
// Without the lock both list the same projects and the loser's
// DeleteProject runs against rows the winner already cascaded, so the RPC
// reports a failure caused entirely by its peer. Reset also owns disk:
// each one RemoveAlls the workspaces tree the other is still walking.
func TestConcurrentResetsBothSucceed(t *testing.T) {
	h, app := newHarnessTestApp(t)

	for round := range 8 {
		if _, err := h.seed(HarnessSeedSpec{Projects: []HarnessSeedProject{
			{Name: "alpha", Repo: &harness.RepoSpec{}},
			{Name: "beta", Repo: &harness.RepoSpec{}},
		}}); err != nil {
			t.Fatalf("round %d: seed: %v", round, err)
		}

		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(len(errs))
		for i := range errs {
			go func() {
				defer wg.Done()
				errs[i] = h.HarnessReset()
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: concurrent reset %d: %v", round, i, err)
			}
		}

		projects, err := app.store.ListProjects()
		if err != nil {
			t.Fatalf("round %d: list projects: %v", round, err)
		}
		if len(projects) != 0 {
			t.Fatalf("round %d: %d projects survived a reset", round, len(projects))
		}
	}
}

// TestHarnessInfoReportsTheSoakAutopilotOutcome pins the latch fix 4
// added. The arming runs on a goroutine that starts after the instance
// has already been published as a soak, so before the latch a failed arm
// was one log line and every tool saw a healthy-looking soak.
func TestHarnessInfoReportsTheSoakAutopilotOutcome(t *testing.T) {
	h, _ := newHarnessTestApp(t)

	info, err := h.HarnessInfo()
	if err != nil {
		t.Fatalf("HarnessInfo: %v", err)
	}
	if info.SoakAutopilot != soakAutopilotOff {
		t.Fatalf("a boot with no --autopilot reports %q, want %q", info.SoakAutopilot, soakAutopilotOff)
	}

	for _, state := range []string{
		soakAutopilotArming,
		soakAutopilotArmed,
		soakAutopilotFailedPrefix + "install soak scenario: no such scenario",
	} {
		h.setSoakAutopilot(state)
		info, err := h.HarnessInfo()
		if err != nil {
			t.Fatalf("HarnessInfo: %v", err)
		}
		if info.SoakAutopilot != state {
			t.Fatalf("SoakAutopilot = %q, want %q", info.SoakAutopilot, state)
		}
	}

	// The key name is consumed by ao-harness; a rename is a wire break.
	if info, err = h.HarnessInfo(); err != nil {
		t.Fatalf("HarnessInfo: %v", err)
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal info: %v", err)
	}
	if !strings.Contains(string(raw), `"soakAutopilot":"failed: `) {
		t.Fatalf("HarnessInfo JSON %s does not carry a soakAutopilot failure", raw)
	}
}
