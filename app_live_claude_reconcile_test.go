package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// registerLiveClaudeSession puts a headless Claude session in the registry
// without a provider process behind it. The sweep only needs the registry
// entry — reconcileSessionConfigStep is the seam that stands in for the wire
// work.
func registerLiveClaudeSession(t *testing.T, app *App, threadID string) {
	t.Helper()
	thread := testThread(threadID)
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(%s) error = %v", threadID, err)
	}
	app.mu.Lock()
	app.sessions[threadID] = session{provider: string(provider.Claude), token: "live-" + threadID}
	app.mu.Unlock()
}

// observeReconcileSteps replaces the per-thread reconcile with a recorder.
func observeReconcileSteps(app *App) chan string {
	steps := make(chan string, 32)
	app.mu.Lock()
	app.reconcileSessionConfigFn = func(threadID string) { steps <- threadID }
	app.mu.Unlock()
	return steps
}

// A settings save that owns a LIVE Claude axis must converge sessions that
// are already running. Prompt overrides are the axis that motivated the live
// path at all: reconcileSettingsOwnedAxes RESOLVES the prompt rather than
// pinning it, so an edited entry lands over set_model.system_prompt — but
// only if something fires the reconcile, and nothing else does on a save.
func TestSettingsSaveReconcilesLiveClaudeSessions(t *testing.T) {
	app := newTestAppWithStore(t)
	registerLiveClaudeSession(t, app, "thread-live-claude")
	steps := observeReconcileSteps(app)

	if _, err := app.UpdateSettings(map[string]any{
		"claudePromptOverrides": []any{
			map[string]any{
				"id":      "override-1",
				"name":    "House style",
				"enabled": true,
				"models":  []any{"claude-fable-5"},
				"prompt":  "Answer in the house style.",
			},
		},
	}); err != nil {
		t.Fatalf("UpdateSettings() error = %v", err)
	}

	select {
	case got := <-steps:
		if got != "thread-live-claude" {
			t.Fatalf("reconciled %q, want thread-live-claude", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a prompt-override save never reconciled the live Claude session")
	}
}

// Each key in liveClaudeSettingsAxes is a separate convergence route
// (control request / deferred restart / live prompt swap), so each one is
// pinned rather than trusting one representative.
func TestEveryLiveClaudeAxisTriggersTheSweep(t *testing.T) {
	for _, key := range liveClaudeSettingsAxes {
		if !patchTouchesLiveClaudeAxis(map[string]any{key: nil}) {
			t.Errorf("patchTouchesLiveClaudeAxis(%q) = false, want true", key)
		}
	}
}

// The inverse half. A spawn-only axis has no live consequence, so firing the
// fan-out for it would put one control request on every live Claude process
// to converge nothing. codexPromptOverrides is the sharp case: the reconcile
// PINS the prompt on every non-Claude provider, so the sweep could only be a
// no-op.
func TestSpawnOnlySettingsDoNotTriggerTheSweep(t *testing.T) {
	for _, key := range []string{
		"codexPromptOverrides",
		"claudeOutputStyle",
		"claudeSubagentLimits",
		"claudeToolMemoryLimit",
		"theme",
	} {
		if patchTouchesLiveClaudeAxis(map[string]any{key: nil}) {
			t.Errorf("patchTouchesLiveClaudeAxis(%q) = true, want false", key)
		}
	}
}

// Coalescing has to hold both halves: no save is lost (every request is
// followed by a sweep that STARTS after it), and N rapid saves cost at most
// two sweeps rather than N. Two saves landing during one in-flight sweep
// therefore buy exactly one more.
func TestLiveClaudeReconcileCoalescesSavesDuringASweep(t *testing.T) {
	app := newTestAppWithStore(t)
	registerLiveClaudeSession(t, app, "thread-coalesce")

	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	sweeps := 0
	app.mu.Lock()
	app.reconcileSessionConfigFn = func(string) {
		app.mu.Lock()
		sweeps++
		first := sweeps == 1
		app.mu.Unlock()
		entered <- struct{}{}
		if first {
			// Hold sweep 1 open so both saves below land while it runs.
			<-release
		}
	}
	app.mu.Unlock()

	app.scheduleLiveClaudeReconcile()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first sweep never started")
	}

	// Two saves while sweep 1 is parked inside the reconcile.
	app.scheduleLiveClaudeReconcile()
	app.scheduleLiveClaudeReconcile()

	close(release)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the coalesced re-run never happened — a save was lost")
	}

	// Nothing more may follow: the two saves fold into ONE re-run.
	select {
	case <-entered:
		t.Fatal("two saves during one sweep produced more than one re-run")
	case <-time.After(200 * time.Millisecond):
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		app.mu.Lock()
		running := app.liveClaudeReconcileRunning
		dirty := app.liveClaudeReconcileDirty
		total := sweeps
		app.mu.Unlock()
		if !running {
			if dirty {
				t.Fatal("the sweep loop exited with the dirty flag still set")
			}
			if total != 2 {
				t.Fatalf("sweeps = %d, want 2 (the in-flight one plus one coalesced re-run)", total)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sweep loop never settled (sweeps = %d)", total)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// B20, the interleaving that the sweep used to miss entirely:
//
//	T0  a Claude spawn snapshots Settings and starts building its session
//	T1  the user saves a live Claude axis; the sweep runs and scans only
//	    the session map, which does not hold the starting thread yet
//	T2  the session registers, carrying T0's settings
//
// Nothing reconciles that thread again — not the spawn (it already read the
// row), not the sweep (it is over), not the watcher (nothing scheduled one).
// The session runs the pre-save config until it is restarted for some other
// reason, which on a long-lived thread can be never.
func TestLiveClaudeSweepCoversASessionStillStarting(t *testing.T) {
	app := newTestAppWithStore(t)
	registerLiveClaudeSession(t, app, "thread-registered")
	// The thread whose start is in flight: present in startingSessions,
	// absent from sessions, exactly as it is between spawn and registration.
	startState, leader := app.sessionManager().beginStart("thread-starting")
	if !leader {
		t.Fatal("beginStart did not lead for a fresh thread")
	}
	defer app.sessionManager().finishStart("thread-starting", startState)

	steps := observeReconcileSteps(app)
	app.reconcileLiveClaudeSessions()

	visited := drainReconcileSteps(steps)
	if !visited["thread-starting"] {
		t.Fatal("the sweep skipped a session still starting — it would run the pre-save settings for its whole life")
	}
	if !visited["thread-registered"] {
		t.Fatal("the sweep skipped the already-registered session")
	}
}

// The registration handoff must not open a hole of its own: a start puts the
// session into the map before it clears the starting entry, so for a moment
// the thread is in BOTH. Reading the two maps under one lock is what turns
// that overlap into "at least once" rather than a chance of neither — and it
// must still be exactly one visit, since the reconcile does wire I/O.
func TestLiveClaudeSweepVisitsAHandingOffThreadExactlyOnce(t *testing.T) {
	app := newTestAppWithStore(t)
	registerLiveClaudeSession(t, app, "thread-handoff")
	startState, leader := app.sessionManager().beginStart("thread-handoff")
	if !leader {
		t.Fatal("beginStart did not lead for a fresh thread")
	}
	defer app.sessionManager().finishStart("thread-handoff", startState)

	steps := observeReconcileSteps(app)
	app.reconcileLiveClaudeSessions()

	var count int
	for range drainReconcileStepList(steps) {
		count++
	}
	if count != 1 {
		t.Fatalf("a thread mid-handoff was reconciled %d times, want exactly 1", count)
	}
}

// A starting thread that turns out to be on ANOTHER provider is swept too —
// an in-flight start has no provider yet, so it cannot be filtered. The cost
// has to be a no-op rather than a wrong reconcile, which is what the
// per-thread step's wait-then-diff gives.
func TestLiveClaudeSweepIncludesStartsOfUnknownProvider(t *testing.T) {
	app := newTestAppWithStore(t)
	app.mu.Lock()
	app.sessions["thread-codex"] = session{provider: string(provider.Codex), token: "codex-1"}
	app.mu.Unlock()
	startState, _ := app.sessionManager().beginStart("thread-unknown")
	defer app.sessionManager().finishStart("thread-unknown", startState)

	steps := observeReconcileSteps(app)
	app.reconcileLiveClaudeSessions()

	visited := drainReconcileSteps(steps)
	if !visited["thread-unknown"] {
		t.Fatal("the sweep skipped an in-flight start whose provider is not yet known")
	}
	if visited["thread-codex"] {
		t.Fatal("a REGISTERED Codex session was swept by the Claude fan-out")
	}
}

func drainReconcileStepList(steps chan string) []string {
	var got []string
	for {
		select {
		case id := <-steps:
			got = append(got, id)
		case <-time.After(50 * time.Millisecond):
			return got
		}
	}
}

func drainReconcileSteps(steps chan string) map[string]bool {
	visited := map[string]bool{}
	for _, id := range drainReconcileStepList(steps) {
		visited[id] = true
	}
	return visited
}

// writeClaudeControlCaptureCLI is a fake `claude` that announces a version
// new enough for a live `set_model.system_prompt` (the swap is gated on
// `system/init.claude_code_version`, and an older build ACKs it without
// applying it), records every control_request it is sent, and answers all of
// them with success. It never spawns anything, reads no provider home, and
// makes no API call (root AGENTS.md §Permanent invariants).
func writeClaudeControlCaptureCLI(t *testing.T) (binary, capture string) {
	t.Helper()
	dir := t.TempDir()
	capture = filepath.Join(dir, "control-requests.jsonl")
	script := `#!/bin/sh
set -eu
printf '%s\n' '{"type":"system","subtype":"init","session_id":"starting-session","model":"claude-opus-5","cwd":"/tmp","tools":[],"claude_code_version":"2.1.237"}'
while IFS= read -r line; do
    case "$line" in
        *'"type":"control_request"'*)
            printf '%s\n' "$line" >> ` + shellQuote(capture) + `
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	binary = filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return binary, capture
}

// waitForCaptured polls the fake CLI's capture file until it holds want.
func waitForCaptured(t *testing.T, capture, want, whatFailed string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		body, err := os.ReadFile(capture)
		if err == nil && strings.Contains(string(body), want) {
			return string(body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (captured control requests: %s)", whatFailed, string(body))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// B20 end to end, on the wire this time: the sweep covering a starting thread
// is only worth anything if the reconcile that follows the start actually
// lands the saved setting on the session that registered.
//
//	T0  a Claude spawn is in flight; it snapshotted Settings already
//	T1  the user saves claudePromptOverrides; the sweep enumerates ids —
//	    `sessions` does not hold this thread yet — and its per-thread
//	    reconcile parks in waitForStartingSession
//	T2  the session registers carrying T0's options and the start clears
//	T3  the parked reconcile diffs the session that actually exists and
//	    swaps the prompt over set_model
//
// The seam here observes the DISPATCH only and then runs the production
// reconcile, which is also what makes the interleaving deterministic: nothing
// registers the session until the sweep has already enumerated its thread ids.
func TestSettingsSaveReachesTheWireOfASessionThatWasStillStarting(t *testing.T) {
	app := newTestAppWithStore(t)
	threadID, workspace := seedPromptOverrideThread(t,
		app, "thread-starting-wire", string(provider.Claude), "claude-opus-5")
	// What the spawn snapshotted: options built BEFORE the save lands.
	launch := promptOverrideOptions(t, app, threadID)
	if launch.SystemPrompt != "" {
		t.Fatalf("launch SystemPrompt = %q, want none — the save is what introduces one", launch.SystemPrompt)
	}

	startState, leader := app.sessionManager().beginStart(threadID)
	if !leader {
		t.Fatal("beginStart did not lead for a fresh thread")
	}

	dispatched := make(chan string, 8)
	app.mu.Lock()
	app.reconcileSessionConfigFn = func(id string) {
		dispatched <- id
		app.reconcileSessionConfig(id)
	}
	app.mu.Unlock()

	const housePrompt = "Answer in the house style."
	if _, err := app.UpdateSettings(map[string]any{
		"claudePromptOverrides": []any{
			map[string]any{
				"id":      "override-1",
				"name":    "House style",
				"enabled": true,
				"models":  []any{"claude-opus-5"},
				"prompt":  housePrompt,
			},
		},
	}); err != nil {
		t.Fatalf("UpdateSettings() error = %v", err)
	}

	select {
	case got := <-dispatched:
		if got != threadID {
			t.Fatalf("sweep dispatched %q, want the starting thread", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the save never dispatched a reconcile for the starting thread")
	}

	// T2, in the order production uses: the session lands in `sessions`
	// first, and only then does the start clear.
	binary, capture := writeClaudeControlCaptureCLI(t)
	initSeen := make(chan struct{})
	var once sync.Once
	sess, err := claude.NewSession(context.Background(), threadID, claude.Config{
		Binary:  binary,
		Model:   "claude-opus-5",
		WorkDir: workspace,
	}, func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventInit {
			once.Do(func() { close(initSeen) })
		}
	})
	if err != nil {
		t.Fatalf("claude.NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	// Register only once the handshake has landed: the prompt swap is
	// version-gated, and a session that has not read its own init line yet
	// would take the restart path for a reason this test is not about.
	select {
	case <-initSeen:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake CLI never announced its version")
	}
	app.mu.Lock()
	app.sessions[threadID] = session{
		provider:   string(provider.Claude),
		token:      "starting-token",
		claude:     sess,
		launchOpts: launch,
		liveness:   newSessionLiveness(time.Now()),
	}
	app.mu.Unlock()
	app.sessionManager().finishStart(threadID, startState)

	body := waitForCaptured(t, capture, housePrompt,
		"the saved prompt override never reached the session that registered after the sweep started")
	if !strings.Contains(body, `"set_model"`) {
		t.Fatalf("prompt landed on something other than set_model: %s", body)
	}
	// The live swap is what the session is running now, so launchOpts has to
	// say so — otherwise the next reconcile re-sends it and a deferred
	// restart would see "already converged" and skip.
	current, ok := app.sessionManager().get(threadID)
	if !ok {
		t.Fatal("session vanished after the reconcile")
	}
	if current.launchOpts.SystemPrompt != housePrompt {
		t.Fatalf("launchOpts.SystemPrompt = %q, want the applied override",
			current.launchOpts.SystemPrompt)
	}
}
