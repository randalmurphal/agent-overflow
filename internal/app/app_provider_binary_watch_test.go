package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerstatus"
	"agent-overflow/internal/testutil"
)

// stubProviderBinaryDetect installs the watcher's version-probe seam for the
// rest of the test. EVERY watcher test needs it: sweepProviderBinaries reads
// both providers, and the un-stubbed seam is provider.DetectProvider, which
// would spawn the fixture's poisoned binary and trip the guard.
func stubProviderBinaryDetect(t *testing.T, versions func(providerName string) string) {
	t.Helper()

	previous := providerBinaryDetectFn
	providerBinaryDetectFn = func(providerName, binaryPath string) provider.ProviderStatus {
		return provider.ProviderStatus{
			Provider:   providerName,
			Installed:  true,
			BinaryPath: binaryPath,
			Status:     "ready",
			Version:    versions(providerName),
		}
	}
	t.Cleanup(func() { providerBinaryDetectFn = previous })
}

func writeProviderBinaryFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// Stat identity is (path, size, mtime); a rewrite inside the same clock
	// tick would otherwise be invisible to the watcher for reasons that
	// have nothing to do with the code under test.
	past := time.Now().Add(-time.Duration(len(body)+1) * time.Minute)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// A quiet tick must cost two stats and nothing else — the whole reason the
// watcher can run every minute is that an unchanged binary is never spawned.
func TestProviderBinaryWatchProbesOnlyWhenTheBinaryChanges(t *testing.T) {
	app := newTestAppWithStore(t)
	binary := filepath.Join(t.TempDir(), "claude")
	writeProviderBinaryFile(t, binary, "exit 0")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	claudeProbes := 0
	stubProviderBinaryDetect(t, func(providerName string) string {
		if providerName == string(provider.Claude) {
			claudeProbes++
		}
		return "2.1.100"
	})

	app.sweepProviderBinaries()
	if claudeProbes != 1 {
		t.Fatalf("baseline tick probed %d times, want 1", claudeProbes)
	}
	app.sweepProviderBinaries()
	if claudeProbes != 1 {
		t.Fatalf("quiet tick re-probed (%d total); an unchanged binary must cost stats only", claudeProbes)
	}

	writeProviderBinaryFile(t, binary, "exit 0 # reinstalled, same release")
	app.sweepProviderBinaries()
	if claudeProbes != 2 {
		t.Fatalf("changed binary probed %d times, want 2", claudeProbes)
	}
	app.sweepProviderBinaries()
	if claudeProbes != 2 {
		t.Fatalf("same-version change was not committed; it re-probes forever (%d)", claudeProbes)
	}
}

// A failed version read must not be recorded as the truth about the binary:
// the next tick has to retry, or one torn read pins a wrong "installed
// version" for the life of the process.
func TestProviderBinaryWatchRetriesAnUnreadableVersion(t *testing.T) {
	app := newTestAppWithStore(t)
	binary := filepath.Join(t.TempDir(), "claude")
	writeProviderBinaryFile(t, binary, "exit 0")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	reported := ""
	probes := 0
	stubProviderBinaryDetect(t, func(providerName string) string {
		if providerName != string(provider.Claude) {
			return "0.149.0"
		}
		probes++
		return reported
	})

	app.sweepProviderBinaries()
	app.sweepProviderBinaries()
	if probes != 2 {
		t.Fatalf("unreadable version probed %d times, want a retry on the next tick", probes)
	}

	reported = "2.1.100"
	app.sweepProviderBinaries()
	app.sweepProviderBinaries()
	if probes != 3 {
		t.Fatalf("probed %d times; a successful read must be committed", probes)
	}
}

// writeClaudeProbeMockAt writes a Claude probe mock (the initialize
// handshake writeProbeMockBinary answers) at a caller-chosen path, with a
// comment that lets a test change the file's BYTES without changing what it
// answers — which is exactly the shape of an in-place CLI upgrade.
func writeClaudeProbeMockAt(t *testing.T, path, noise string) {
	t.Helper()
	response := `{"type":"control_response","response":{"subtype":"success","request_id":"ao-probe-init",` +
		`"response":{"account":{"subscriptionType":"pro","tokenSource":"oauth"}}}}`
	writeProviderBinaryFile(t, path,
		"# "+noise+"\nread -r _ || true\nprintf '%s\\n' '"+response+"'\nexit 0")
}

// The upgrade path: a changed binary reporting a different version refreshes
// the account/model catalog, and only a SUCCESSFUL refresh commits the new
// version — otherwise the picker is left describing a binary that is gone
// and nothing ever retries.
func TestProviderBinaryUpgradeRefreshesTheCatalogBeforeCommitting(t *testing.T) {
	resetClaudeProbeCacheForTest()
	app := newTestAppWithStore(t)

	// The recheck runs the real zero-token probe, so the configured binary
	// is a mock that answers the initialize handshake and exits.
	binary := filepath.Join(t.TempDir(), "claude")
	writeClaudeProbeMockAt(t, binary, "2.1.100")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	version := "2.1.100"
	probes := 0
	stubProviderBinaryDetect(t, func(providerName string) string {
		if providerName != string(provider.Claude) {
			return "0.149.0"
		}
		probes++
		return version
	})

	app.sweepProviderBinaries()

	// An upgrade whose account re-probe fails: the version must NOT be
	// recorded, so the next tick tries the whole thing again.
	version = "2.1.200"
	writeProviderBinaryFile(t, binary, "exit 1")
	app.sweepProviderBinaries()
	if probes != 2 {
		t.Fatalf("changed binary probed %d times, want 2", probes)
	}
	if installed, _ := app.providerBinaries.lookupInstalled(string(provider.Claude)); installed.version != "2.1.100" {
		t.Fatalf("installed version = %q; a failed catalog refresh must not commit", installed.version)
	}
	app.sweepProviderBinaries()
	if probes != 3 {
		t.Fatalf("probed %d times, want the failed refresh retried", probes)
	}

	resetClaudeProbeCacheForTest()
	writeClaudeProbeMockAt(t, binary, "2.1.200")
	app.sweepProviderBinaries()
	installed, ok := app.providerBinaries.lookupInstalled(string(provider.Claude))
	if !ok || installed.version != "2.1.200" {
		t.Fatalf("installed version = %q (known=%v), want the upgraded 2.1.200", installed.version, ok)
	}
	probed := probes
	app.sweepProviderBinaries()
	if probes != probed {
		t.Fatalf("the upgrade was not committed; it re-probes forever (%d)", probes)
	}
}

// writeCodexProbeMockAt writes a `codex app-server` stand-in that answers the
// zero-token probe: drain the four request lines, then answer the id=2
// account/rateLimits/read. `noise` changes the file's bytes so a rewrite is
// a new stat identity.
func writeCodexProbeMockAt(t *testing.T, path, noise string) {
	t.Helper()
	response := `{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"limitId":"codex","planType":"pro","primary":{},"secondary":{}}}}`
	writeProviderBinaryFile(t, path,
		"# "+noise+"\nread -r _ || true\nread -r _ || true\nread -r _ || true\nread -r _ || true\n"+
			"printf '%s\\n' '"+response+"'\nexit 0")
}

// The Codex leg of the same path. Its two calls are provider-specific and
// neither is exercised by the Claude test: the `model/list` cache is keyed
// by binary PATH, which an in-place upgrade does not change, so it must be
// dropped explicitly; and the recheck runs the app-server probe, whose
// failure must leave the old version uncommitted so the tick retries.
func TestProviderBinaryUpgradeRefreshesTheCodexCatalogBeforeCommitting(t *testing.T) {
	resetClaudeProbeCacheForTest()
	app := newTestAppWithStore(t)

	binary := filepath.Join(t.TempDir(), "codex")
	writeCodexProbeMockAt(t, binary, "0.149.0")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("set codex binary: %v", err)
	}
	lists := 0
	app.providerDiscoveryCaches.CodexModels = codexmodels.NewWith(time.Minute, func(context.Context, string) ([]provider.ModelInfo, error) {
		lists++
		return []provider.ModelInfo{{Slug: "gpt-5", Provider: string(provider.Codex)}}, nil
	}, time.Now)
	if _, err := app.codexModelsForBinary(context.Background(), binary); err != nil {
		t.Fatalf("prime codex catalog: %v", err)
	}
	if lists != 1 {
		t.Fatalf("catalog listed %d times priming, want 1", lists)
	}

	version := "0.149.0"
	probes := 0
	stubProviderBinaryDetect(t, func(providerName string) string {
		if providerName != string(provider.Codex) {
			return "2.1.100"
		}
		probes++
		return version
	})

	app.sweepProviderBinaries()
	if _, _, cached := app.cachedCodexModelsForBinary(binary); !cached {
		t.Fatal("baseline sighting dropped the codex catalog; only an upgrade may")
	}

	// Upgrade whose account re-probe fails: not committed, retried next tick.
	version = "0.150.0"
	writeProviderBinaryFile(t, binary, "exit 1")
	app.sweepProviderBinaries()
	if probes != 2 {
		t.Fatalf("changed binary probed %d times, want 2", probes)
	}
	if installed, _ := app.providerBinaries.lookupInstalled(string(provider.Codex)); installed.version != "0.149.0" {
		t.Fatalf("installed version = %q; a failed catalog refresh must not commit", installed.version)
	}
	app.sweepProviderBinaries()
	if probes != 3 {
		t.Fatalf("probed %d times, want the failed refresh retried", probes)
	}

	// Upgrade whose re-probe succeeds: committed, and the path-keyed model
	// cache no longer answers for the old build.
	resetClaudeProbeCacheForTest()
	writeCodexProbeMockAt(t, binary, "0.150.0")
	app.sweepProviderBinaries()
	installed, ok := app.providerBinaries.lookupInstalled(string(provider.Codex))
	if !ok || installed.version != "0.150.0" {
		t.Fatalf("installed version = %q (known=%v), want the upgraded 0.150.0", installed.version, ok)
	}
	if _, _, cached := app.cachedCodexModelsForBinary(binary); cached {
		t.Fatal("codex model cache still answers for the old binary after the upgrade")
	}
	if _, err := app.codexModelsForBinary(context.Background(), binary); err != nil {
		t.Fatalf("relist codex catalog: %v", err)
	}
	if lists != 2 {
		t.Fatalf("catalog listed %d times, want a fresh list after the upgrade", lists)
	}
	probed := probes
	app.sweepProviderBinaries()
	if probes != probed {
		t.Fatalf("the upgrade was not committed; it re-probes forever (%d)", probes)
	}
}

// reconcileStale is the transition ledger. Only changes are reported: a
// thread that is still stale with the same two versions must not re-emit on
// every tick, and a thread that leaves the set must be reported once.
func TestReconcileStaleReportsTransitionsOnly(t *testing.T) {
	state := &appProviderBinaryWatchState{}
	stale := staleProviderBinary{provider: "claude", session: "2.1.100", installed: "2.1.200"}

	entered, cleared := state.reconcileStale(map[string]staleProviderBinary{"t1": stale})
	if len(entered) != 1 || entered["t1"] != stale || len(cleared) != 0 {
		t.Fatalf("first flag: entered=%v cleared=%v", entered, cleared)
	}

	entered, cleared = state.reconcileStale(map[string]staleProviderBinary{"t1": stale})
	if len(entered) != 0 || len(cleared) != 0 {
		t.Fatalf("unchanged tick emitted: entered=%v cleared=%v", entered, cleared)
	}

	moved := staleProviderBinary{provider: "claude", session: "2.1.100", installed: "2.1.300"}
	entered, _ = state.reconcileStale(map[string]staleProviderBinary{"t1": moved})
	if len(entered) != 1 || entered["t1"] != moved {
		t.Fatalf("a second upgrade must restate the banner: entered=%v", entered)
	}

	entered, cleared = state.reconcileStale(nil)
	if len(entered) != 0 || cleared["t1"] != "claude" {
		t.Fatalf("session gone: entered=%v cleared=%v", entered, cleared)
	}
	if _, still := state.stale["t1"]; still {
		t.Fatal("a cleared thread must leave the flagged set")
	}
}

// End to end over a live session: the banner is thread-scoped, carries both
// versions, hydrates through GetThreadLiveState for a reconnecting webview,
// and is withdrawn when the versions agree again.
func TestStaleBinaryFlagsTheLiveThreadAndHydrates(t *testing.T) {
	app, bus := setupE2EApp(t)

	var mu sync.Mutex
	var statuses []providerstatus.Event
	app.testEmitHook = func(name string, data any) {
		if name != "provider:status" {
			return
		}
		event, ok := data.(providerstatus.Event)
		if !ok {
			return
		}
		mu.Lock()
		statuses = append(statuses, event)
		mu.Unlock()
	}
	takeStatuses := func() []providerstatus.Event {
		mu.Lock()
		defer mu.Unlock()
		taken := statuses
		statuses = nil
		return taken
	}

	// The codex leg of every sweep still reaches the seam.
	stubProviderBinaryDetect(t, func(string) string { return "0.149.0" })

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
		`{"type":"system","subtype":"init","session_id":"sess-stale","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"2.1.100"}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	}})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(thread.ID, "hi", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)

	// Seed the installed version at the binary's CURRENT identity, so the
	// sweep's refresh half is a no-op and the test is about staleness only.
	identity, ok := app.resolveProviderBinaryIdentity(string(provider.Claude))
	if !ok {
		t.Fatal("could not resolve the mock claude binary")
	}
	app.providerBinaries.storeInstalled(string(provider.Claude), providerBinaryVersion{
		identity: identity,
		version:  "2.1.200",
	})

	app.sweepProviderBinaries()

	flagged := takeStatuses()
	if len(flagged) != 1 {
		t.Fatalf("provider:status emissions = %d, want exactly the one stale banner: %+v", len(flagged), flagged)
	}
	event := flagged[0]
	if event.Kind != providerstatus.KindBinaryStale {
		t.Errorf("Kind = %q, want %q", event.Kind, providerstatus.KindBinaryStale)
	}
	if event.ThreadID != thread.ID {
		t.Errorf("ThreadID = %q, want the stale thread %q", event.ThreadID, thread.ID)
	}
	if event.Provider != string(provider.Claude) {
		t.Errorf("Provider = %q, want claude", event.Provider)
	}
	if event.SessionVersion != "2.1.100" || event.InstalledVersion != "2.1.200" {
		t.Errorf("versions = (%q, %q), want (2.1.100, 2.1.200)", event.SessionVersion, event.InstalledVersion)
	}
	if !event.Actionable || event.Message == "" {
		t.Errorf("a restartable banner must be actionable and explain itself: %+v", event)
	}

	app.sweepProviderBinaries()
	if repeats := takeStatuses(); len(repeats) != 0 {
		t.Fatalf("an unchanged stale thread re-emitted: %+v", repeats)
	}

	live, err := app.GetThreadLiveState(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadLiveState: %v", err)
	}
	if live.SessionCLIVersion != "2.1.100" || live.InstalledCLIVersion != "2.1.200" {
		t.Errorf("live state versions = (%q, %q), want the flagged pair",
			live.SessionCLIVersion, live.InstalledCLIVersion)
	}

	// The flag is pinned to the session that earned it: once that session
	// is gone the hydration read is empty BEFORE the next tick notices, so
	// a pane switch right after "Restart session" cannot resurrect the
	// banner on the replacement session. No clear event is owed here (the
	// disconnect is the frontend's clear), and the tick that follows must
	// not emit one either — the thread already left the flagged set.
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	live, err = app.GetThreadLiveState(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadLiveState after stop: %v", err)
	}
	if live.SessionCLIVersion != "" || live.InstalledCLIVersion != "" {
		t.Errorf("live state still reports versions after the session stopped: %+v", live)
	}
	if _, still := app.providerBinaries.stale[thread.ID]; still {
		t.Error("a stopped session must leave the flagged set immediately")
	}
	takeStatuses()
	app.sweepProviderBinaries()
	if late := takeStatuses(); len(late) != 0 {
		t.Fatalf("tick after a forgotten thread emitted: %+v", late)
	}

	// Back on a live session running the old build, then the user restarts
	// the CLI onto the session's build (or the session onto the new one):
	// the banner is raised and then withdrawn.
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession again: %v", err)
	}
	if err := app.SendMessage(thread.ID, "hi again", nil); err != nil {
		t.Fatalf("SendMessage again: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	app.sweepProviderBinaries()
	if again := takeStatuses(); len(again) != 1 || again[0].Kind != providerstatus.KindBinaryStale {
		t.Fatalf("restarted stale session emissions = %+v, want one fresh binary_stale", again)
	}
	app.providerBinaries.storeInstalled(string(provider.Claude), providerBinaryVersion{
		identity: identity,
		version:  "2.1.100",
	})
	app.sweepProviderBinaries()

	cleared := takeStatuses()
	if len(cleared) != 1 {
		t.Fatalf("clear emissions = %d, want one: %+v", len(cleared), cleared)
	}
	if cleared[0].Status != "ready" || cleared[0].ThreadID != thread.ID {
		t.Errorf("clear event = %+v, want a thread-scoped ready", cleared[0])
	}
	live, err = app.GetThreadLiveState(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadLiveState: %v", err)
	}
	if live.SessionCLIVersion != "" || live.InstalledCLIVersion != "" {
		t.Errorf("live state still reports versions after the clear: %+v", live)
	}
}

// A session that never stated its version is UNKNOWN, not stale. Flagging it
// would tell users to restart healthy sessions on every provider upgrade.
func TestUnknownSessionVersionIsNeverStale(t *testing.T) {
	app := newTestAppWithStore(t)
	stubProviderBinaryDetect(t, func(string) string { return "2.1.200" })
	app.providerBinaries.storeInstalled(string(provider.Claude), providerBinaryVersion{version: "2.1.200"})

	var mu sync.Mutex
	emissions := 0
	app.testEmitHook = func(name string, _ any) {
		if name == "provider:status" {
			mu.Lock()
			emissions++
			mu.Unlock()
		}
	}
	// A registered session with no provider handle reports no version.
	app.sessionManager().put("thread-unknown-version", session{Provider: string(provider.Claude)})

	app.reconcileStaleProviderSessions()

	mu.Lock()
	defer mu.Unlock()
	if emissions != 0 {
		t.Fatalf("provider:status emissions = %d, want none for an unknown version", emissions)
	}
}
