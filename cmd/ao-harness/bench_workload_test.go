package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The workload table and the fixtures behind it. Nothing here boots
// anything: a workload's REGISTRATION and its seed are plain data, and both
// are things a caller finds out about only by running a bench for real.

func TestBenchWorkloadTableIsComplete(t *testing.T) {
	names := map[string]bool{}
	for _, workload := range benchWorkloads() {
		if names[workload.Name] {
			t.Errorf("workload %q is registered twice", workload.Name)
		}
		names[workload.Name] = true
		if workload.Summary == "" {
			t.Errorf("workload %q has no summary; it is what `bench` prints for an unknown name", workload.Name)
		}
		if workload.seed == nil {
			t.Errorf("workload %q seeds no fixture", workload.Name)
		}
		if workload.drive == nil {
			t.Errorf("workload %q drives nothing", workload.Name)
		}
	}
	for _, want := range []string{"burst-stream", "giant-turn", "subagent-fanout", "many-threads", "multi-pane-stream"} {
		if !names[want] {
			t.Errorf("workload %q is missing from the table", want)
		}
	}
}

// The multi-pane workload is the only one with a PREPARE step, and the
// split is the whole point: mounting three timelines is setup, and folding
// it into the measured window would dominate the first second of every
// repeat. A workload that grew a prepare by accident would silently move
// that cost back inside.
func TestOnlyMultiPaneStreamPrepares(t *testing.T) {
	for _, workload := range benchWorkloads() {
		prepares := workload.prepare != nil
		if want := workload.Name == "multi-pane-stream"; prepares != want {
			t.Errorf("workload %q prepare = %t, want %t", workload.Name, prepares, want)
		}
	}
}

// It streams, so it needs a scenario: a workload that drove a turn with no
// script installed would measure whatever the mock happened to be holding.
func TestMultiPaneStreamRunsAScenario(t *testing.T) {
	workload, err := benchWorkloadByName("multi-pane-stream")
	if err != nil {
		t.Fatal(err)
	}
	if workload.Scenario == "" {
		t.Fatal("multi-pane-stream drives a turn with no scenario")
	}
}

func TestUnknownWorkloadNamesTheOnesThatExist(t *testing.T) {
	_, err := benchWorkloadByName("multi-pane")
	if err == nil {
		t.Fatal("an unknown workload was accepted")
	}
	if !strings.Contains(err.Error(), "multi-pane-stream") {
		t.Errorf("the refusal does not name the real workloads: %v", err)
	}
}

// One thread per pane, and each with a completed turn — without one
// App.ListThreads hides the row entirely and the bench cannot open it.
func TestSeedMultiPaneThreadsGivesEveryPaneAThread(t *testing.T) {
	workload, err := benchWorkloadByName("multi-pane-stream")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := workload.seed(&benchRun{workload: workload})
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Projects []struct {
			Name    string `json:"name"`
			Threads []struct {
				Title string `json:"title"`
				Turns []struct {
					Items []json.RawMessage `json:"items"`
				} `json:"turns"`
			} `json:"threads"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Projects) != 1 {
		t.Fatalf("seed produced %d projects, want 1", len(spec.Projects))
	}
	threads := spec.Projects[0].Threads
	if len(threads) != benchMultiPaneCount {
		t.Fatalf("seed produced %d threads, want %d", len(threads), benchMultiPaneCount)
	}
	seen := map[string]bool{}
	for _, thread := range threads {
		if seen[thread.Title] {
			t.Errorf("two threads share the title %q; the pane strip would be unreadable", thread.Title)
		}
		seen[thread.Title] = true
		if len(thread.Turns) == 0 || len(thread.Turns[0].Items) == 0 {
			t.Errorf("thread %q carries no completed turn, so ListThreads hides it", thread.Title)
		}
	}
}

// `draining` and `smoothers` are two different observations and the wait
// needs BOTH clear: a pane whose last smoother has been disposed can still
// be holding the reveal gate for the row about to start.
func TestRevealDrainEmptyNeedsBothCounters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drain revealDrain
		empty bool
	}{
		{"nothing at all", revealDrain{Panes: 2}, true},
		{"a live smoother", revealDrain{Panes: 2, Draining: 1, Smoothers: 3}, false},
		{"a standing gate with no smoother", revealDrain{Panes: 2, Draining: 1, Boundaries: 1}, false},
		{"a smoother nothing counted as draining", revealDrain{Panes: 1, Smoothers: 1}, false},
		{"no panes at all", revealDrain{}, true},
	} {
		if got := tc.drain.empty(); got != tc.empty {
			t.Errorf("%s: empty() = %t, want %t", tc.name, got, tc.empty)
		}
	}
}

// The flag exists to pick a DOOR, so a typo next to it must be a refusal
// rather than a plain open in the focused pane — the two produce different
// pane strips and only one of them is what the caller asked for.
func TestUIOpenRefusesStrayPositionalsBesideNewPane(t *testing.T) {
	code, _, stderr := run(t, "ui", "open", "--thread", "abc", "--new-pane", "extra")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "--new-pane") {
		t.Errorf("the refusal does not mention the flag it takes:\n%s", stderr)
	}
}
