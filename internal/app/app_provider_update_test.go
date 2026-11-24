package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/providerstatus"
	"agent-overflow/internal/providerupdate"
	"agent-overflow/internal/providerupdate/updatefake"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"

	"github.com/google/uuid"
)

// providerUpdateFrames records provider:updates frames and wakes waiters.
type providerUpdateFrames struct {
	mu     sync.Mutex
	frames [][]providerupdate.Status
	notify chan struct{}
}

func watchProviderUpdates(app *App) *providerUpdateFrames {
	w := &providerUpdateFrames{notify: make(chan struct{}, 1)}
	app.testEmitHook = func(name string, data any) {
		if name != string(eventchan.ProviderUpdates) {
			return
		}
		w.mu.Lock()
		w.frames = append(w.frames, data.([]providerupdate.Status))
		w.mu.Unlock()
		select {
		case w.notify <- struct{}{}:
		default:
		}
	}
	return w
}

func (w *providerUpdateFrames) wait(t *testing.T, want func(providerupdate.Status) bool, providerName string) providerupdate.Status {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		w.mu.Lock()
		frames := w.frames
		w.mu.Unlock()
		for _, frame := range frames {
			for _, status := range frame {
				if status.Provider == providerName && want(status) {
					return status
				}
			}
		}
		select {
		case <-w.notify:
		case <-deadline:
			t.Fatalf("no %s frame matched: %+v", providerName, frames)
		}
	}
}

func providerUpdateStatus(t *testing.T, statuses []providerupdate.Status, providerName string) providerupdate.Status {
	t.Helper()
	for _, status := range statuses {
		if status.Provider == providerName {
			return status
		}
	}
	t.Fatalf("no %s in %+v", providerName, statuses)
	return providerupdate.Status{}
}

// An npm install names its launcher, which tells Codex's own updater how it
// was installed; the native binary sessions run cannot.
func TestProviderUpdateBinaryNamesTheNpmLauncher(t *testing.T) {
	app := newTestAppWithStore(t)
	f := newCodexNpmFixture(t, t.TempDir())
	writeCodexProbeMockAt(t, f.native, "native")
	writeCodexProbeMockAt(t, f.wrapper, "wrapper")
	putCodexOnPath(t, app, f)

	binary := app.providerUpdateBinary(string(provider.Codex))
	if binary.Path != nativePath(t, f) || binary.Launcher != realPath(t, f.wrapper) {
		t.Fatalf("codex = %+v, want the native binary %q started by the npm launcher %q", binary, nativePath(t, f), realPath(t, f.wrapper))
	}
	if binary.ResolvedPath != nativePath(t, f) {
		t.Errorf("resolved path = %q, want the native binary %q", binary.ResolvedPath, nativePath(t, f))
	}

	explicit := filepath.Join(t.TempDir(), "codex")
	writeProviderBinaryFile(t, explicit, "exit 0")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": explicit}); err != nil {
		t.Fatal(err)
	}
	if binary := app.providerUpdateBinary(string(provider.Codex)); binary.Path != explicit || binary.Launcher != "" || binary.ResolvedPath != realPath(t, explicit) {
		t.Fatalf("explicit codex = %+v, want %q as configured", binary, explicit)
	}

	claude := filepath.Join(t.TempDir(), "claude")
	writeProviderBinaryFile(t, claude, "exit 0")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": claude}); err != nil {
		t.Fatal(err)
	}
	if binary := app.providerUpdateBinary(string(provider.Claude)); binary.Path != claude || binary.Launcher != "" || binary.ResolvedPath != realPath(t, claude) {
		t.Fatalf("claude = %+v", binary)
	}
	if binary := app.providerUpdateBinary("gemini"); binary != (providerupdate.Binary{}) {
		t.Errorf("an unknown provider resolved %+v", binary)
	}
}

// The installed version is the binary watch's: a probe only when the file
// changed, recorded where sessions and the stale banner read it.
func TestProviderUpdateInstalledVersionGoesThroughTheBinaryWatch(t *testing.T) {
	app := newTestAppWithStore(t)
	binary := filepath.Join(t.TempDir(), "claude")
	writeProviderBinaryFile(t, binary, "exit 0")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	probes := 0
	stubProviderBinaryDetect(t, func(providerName string) string {
		if providerName == string(provider.Claude) {
			probes++
		}
		return "2.1.100"
	})
	for range 2 {
		if got := app.providerUpdateInstalledVersion(string(provider.Claude)); got != "2.1.100" {
			t.Fatalf("installed = %q", got)
		}
	}
	if probes != 1 {
		t.Errorf("probed %d times; an unchanged binary is read from the watch", probes)
	}
	if entry, ok := app.providerBinaries.lookupInstalled(string(provider.Claude)); !ok || entry.version != "2.1.100" {
		t.Errorf("watch entry = %+v (%v)", entry, ok)
	}
}

// A finished run sweeps the provider binaries, so the new binary is
// recorded (its catalog refreshed first) and the outcome reads it at once.
func TestProviderUpdateRunSweepsTheUpdatedBinary(t *testing.T) {
	resetClaudeProbeCacheForTest(t)
	app := newTestAppWithStore(t)
	frames := watchProviderUpdates(app)
	binary := filepath.Join(t.TempDir(), "claude")
	writeClaudeProbeMockAt(t, binary, "99.0.0")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	version := updatefake.Baseline
	stubProviderBinaryDetect(t, func(providerName string) string {
		if providerName != string(provider.Claude) {
			return "0.149.0"
		}
		mu.Lock()
		defer mu.Unlock()
		return version
	})
	if err := app.providerUpdateFeed.SetLatest("99.1.0"); err != nil {
		t.Fatal(err)
	}
	app.providerUpdateRunner = func(_ context.Context, command providerupdate.Command) (providerupdate.Output, error) {
		if command.Binary != binary {
			return providerupdate.Output{}, errors.New("unexpected binary " + command.Binary)
		}
		switch command.Args[0] {
		case "doctor":
			return providerupdate.Output{Text: "Auto-update channel: latest\n"}, nil
		case "update":
			writeClaudeProbeMockAt(t, binary, "99.1.0")
			mu.Lock()
			version = "99.1.0"
			mu.Unlock()
			return providerupdate.Output{Text: "Successfully updated from 99.0.0 to version 99.1.0"}, nil
		}
		return providerupdate.Output{}, errors.New("unexpected command")
	}

	statuses, err := app.CheckProviderUpdates()
	if err != nil {
		t.Fatal(err)
	}
	if claude := providerUpdateStatus(t, statuses, string(provider.Claude)); !claude.Available || claude.LatestVersion != "99.1.0" {
		t.Fatalf("claude = %+v, want 99.1.0 offered", claude)
	}
	if err := app.RunProviderUpdate(callSteppedUp("owner"), string(provider.Claude)); err != nil {
		t.Fatalf("RunProviderUpdate: %v", err)
	}
	claude := frames.wait(t, func(s providerupdate.Status) bool {
		return s.Run.Phase == providerupdate.PhaseSucceeded || s.Run.Phase == providerupdate.PhaseFailed
	}, string(provider.Claude))
	if claude.Run.Phase != providerupdate.PhaseSucceeded || claude.Run.ToVersion != "99.1.0" || claude.Available {
		t.Fatalf("claude = %+v, want the run to read the swept 99.1.0", claude)
	}
	entry, ok := app.providerBinaries.lookupInstalled(string(provider.Claude))
	if !ok || entry.version != "99.1.0" {
		t.Fatalf("watch entry = %+v (%v); the run did not sweep the new binary", entry, ok)
	}
	if app.installedProviderVersion(string(provider.Claude)) != "99.1.0" {
		t.Error("the swept identity is not the binary on disk")
	}
}

// The in-method step-up recheck: a session call without a fresh proof
// reaches no update.
func TestRunProviderUpdateRefusesASessionWithNoProvenStepUp(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	ran := false
	app.providerUpdateRunner = func(context.Context, providerupdate.Command) (providerupdate.Output, error) {
		ran = true
		return providerupdate.Output{}, nil
	}
	err := app.RunProviderUpdate(callFrom("session-under-test", false), string(provider.Claude))
	if frame, ok := transport.AuthzFrame(err); !ok || frame.Code != transport.ErrCodeStepUpRequired {
		t.Fatalf("RunProviderUpdate without proof = %v, want a step-up refusal", err)
	}
	// With the proof the transport resolved, the call reaches the service,
	// which refuses for want of a known update.
	if err := app.RunProviderUpdate(callSteppedUp("session-under-test"), string(provider.Claude)); !errors.Is(err, providerupdate.ErrNoUpdate) {
		t.Fatalf("RunProviderUpdate with proof = %v, want ErrNoUpdate", err)
	}
	if ran {
		t.Fatal("a refused call ran a command")
	}
}

// Every App has a service, including a bare literal, and its bound methods
// answer after Shutdown without starting anything.
func TestProviderUpdateMethodsOnABareAppAndAfterShutdown(t *testing.T) {
	t.Parallel()
	app := &App{}
	statuses, err := app.GetProviderUpdates()
	if err != nil || len(statuses) != 3 || statuses[0].Run.Phase != providerupdate.PhaseIdle {
		t.Fatalf("GetProviderUpdates = %+v, %v", statuses, err)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := app.RunProviderUpdate(context.Background(), string(provider.Claude)); !errors.Is(err, providerupdate.ErrShuttingDown) {
		t.Fatalf("RunProviderUpdate after Shutdown = %v", err)
	}
	if _, err := app.CheckProviderUpdates(); err != nil {
		t.Fatal(err)
	}
}

// A test binary refuses the real runner and the real release hosts unless
// a fake is assigned, whatever fixture built the App.
func TestProviderUpdatesRefuseRealCommandsAndHostsInTests(t *testing.T) {
	t.Parallel()
	app := &App{}
	_, err := app.providerUpdateCommand(context.Background(), providerupdate.Command{Binary: "claude", Args: []string{"update"}})
	if err == nil || !strings.Contains(err.Error(), "assign app.providerUpdateRunner a fake") {
		t.Fatalf("runner = %v, want the test-binary refusal", err)
	}
	req, err := http.NewRequest(http.MethodGet, providerupdate.CodexNPMLatestURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.providerUpdateClient().Do(req); !errors.Is(err, errProviderUpdateNetwork) {
		t.Fatalf("client = %v, want the test-binary refusal", err)
	}
}

// An isolated boot answers the release hosts from its fake feed and refuses
// every other host; the default feed offers no update.
func TestIsolatedBootAnswersTheReleaseHostsInProcess(t *testing.T) {
	t.Parallel()
	app := &App{}
	ConfigureIsolation(app, IsolationConfig{})
	if app.providerUpdateFeed == nil {
		t.Fatal("ConfigureIsolation installed no release feed")
	}
	get := func(url string) (string, error) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := app.providerUpdateClient().Do(req)
		if err != nil {
			return "", err
		}
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); err == nil {
			err = closeErr
		}
		return string(body), err
	}
	body, err := get(providerupdate.ClaudeChannelURL("latest"))
	if err != nil || strings.TrimSpace(body) != updatefake.Baseline {
		t.Fatalf("channel = %q, %v; want the default %s", body, err, updatefake.Baseline)
	}
	if err := app.providerUpdateFeed.SetLatest("99.1.0"); err != nil {
		t.Fatal(err)
	}
	if body, err := get(providerupdate.CodexNPMLatestURL); err != nil || !strings.Contains(body, `"version":"99.1.0"`) {
		t.Fatalf("npm manifest = %q, %v", body, err)
	}
	if body, err := get(providerupdate.CodexLatestReleaseURL); err != nil || !strings.Contains(body, `"tag_name":"rust-v99.1.0"`) {
		t.Fatalf("latest Codex release = %q, %v", body, err)
	}
	if _, err := get(providerupdate.BrewCaskURL("codex")); !errors.Is(err, errIsolatedNetwork) {
		t.Errorf("brew cask = %v, want the isolated refusal", err)
	}
}

// Through a test fixture's isolation the whole check runs in process: the
// stubbed runner fails Claude's doctor loudly, and the fixture's Codex,
// which no installer owns, has no updater to ask.
func TestFixtureIsolationAnswersChecksInProcess(t *testing.T) {
	app := newTestAppWithStore(t)
	stubProviderBinaryDetect(t, func(providerName string) string { return updatefake.Baseline })
	statuses, err := app.CheckProviderUpdates()
	if err != nil {
		t.Fatal(err)
	}
	claude := providerUpdateStatus(t, statuses, string(provider.Claude))
	if claude.Available || !strings.Contains(claude.CheckError, "stubbed in tests") {
		t.Errorf("claude = %+v, want the stubbed runner reported", claude)
	}
	codex := providerUpdateStatus(t, statuses, string(provider.Codex))
	if codex.Available || !strings.Contains(codex.CheckError, "has no updater") || codex.InstalledVersion != updatefake.Baseline {
		t.Errorf("codex = %+v, want its installed version and no updater reported", codex)
	}
}

// The sweep after a run reconciles live sessions against the binary at
// once: a session running an older build than the installed one is flagged
// without waiting for the watch tick.
func TestProviderUpdateRunReconcilesStaleSessions(t *testing.T) {
	app, bus := setupE2EApp(t)
	frames := watchProviderUpdates(app)
	var mu sync.Mutex
	var stale []providerstatus.Event
	updates := app.testEmitHook
	app.testEmitHook = func(name string, data any) {
		updates(name, data)
		if event, ok := data.(providerstatus.Event); ok && event.Kind == providerstatus.KindBinaryStale {
			mu.Lock()
			stale = append(stale, event)
			mu.Unlock()
		}
	}
	stubProviderBinaryDetect(t, func(string) string { return "0.149.0" })

	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatal(err)
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
		`{"type":"system","subtype":"init","session_id":"sess-update","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"99.0.0"}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	}})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	if err := app.startSessionTakingLock(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "hi", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatal(err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	// The binary on disk already reads as a later build than the session's,
	// which the watch has not reconciled yet.
	identity, ok := app.resolveProviderBinaryIdentity(string(provider.Claude))
	if !ok {
		t.Fatal("could not resolve the mock claude binary")
	}
	app.providerBinaries.storeInstalled(string(provider.Claude), providerBinaryVersion{identity: identity, version: "99.0.1"})

	if err := app.providerUpdateFeed.SetLatest("99.1.0"); err != nil {
		t.Fatal(err)
	}
	app.providerUpdateRunner = func(_ context.Context, command providerupdate.Command) (providerupdate.Output, error) {
		if command.Args[0] == "doctor" {
			return providerupdate.Output{Text: "Auto-update channel: latest\n"}, nil
		}
		return providerupdate.Output{Text: "Update failed quietly"}, nil
	}
	if _, err := app.CheckProviderUpdates(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	before := len(stale)
	mu.Unlock()
	if before != 0 {
		t.Fatal("flagged before the run")
	}
	if err := app.RunProviderUpdate(context.Background(), string(provider.Claude)); err != nil {
		t.Fatal(err)
	}
	claude := frames.wait(t, func(s providerupdate.Status) bool { return s.Run.Phase == providerupdate.PhaseFailed }, string(provider.Claude))
	if !strings.Contains(claude.Run.Error, "still 99.0.1") {
		t.Errorf("run = %+v", claude.Run)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stale) != 1 || stale[0].ThreadID != thread.ID || stale[0].SessionVersion != "99.0.0" || stale[0].InstalledVersion != "99.0.1" {
		t.Fatalf("stale banners = %+v, want the live thread flagged by the run's sweep", stale)
	}
}

// The update check reads the selected Claude login through the account
// manager: past its refresh deadline it runs no `claude doctor` and reports
// a sign-in, and a renewed login is checked again.
func TestProviderUpdateCheckRunsNoDoctorOnAnExpiredClaudeLogin(t *testing.T) {
	app := newTestAppWithStore(t)
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	attachProviderAccountStoresForTest(t, app, accounts, newTestProviderCredentials(t, t.TempDir()))
	stubProviderBinaryDetect(t, func(string) string { return updatefake.Baseline })
	canonical, err := providerCredentialsForTest(t, app).ActiveCredentialPath(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(canonical), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLogin := func(expiresAt time.Time) {
		t.Helper()
		credential := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
			expiresAt.UnixMilli(), expiresAt.UnixMilli())
		if err := os.WriteFile(canonical, []byte(credential), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	doctors := 0
	app.providerUpdateRunner = func(_ context.Context, command providerupdate.Command) (providerupdate.Output, error) {
		mu.Lock()
		defer mu.Unlock()
		doctors++
		return providerupdate.Output{Text: "Auto-update channel: latest\n"}, nil
	}

	writeLogin(time.Now().Add(-time.Hour))
	statuses, err := app.CheckProviderUpdates()
	if err != nil {
		t.Fatal(err)
	}
	if claude := providerUpdateStatus(t, statuses, string(provider.Claude)); !claude.SignInRequired || claude.CheckError != "" {
		t.Fatalf("claude = %+v, want a sign-in required", claude)
	}
	mu.Lock()
	if doctors != 0 {
		t.Fatalf("ran %d commands against an expired login", doctors)
	}
	mu.Unlock()

	writeLogin(time.Now().Add(30 * 24 * time.Hour))
	statuses, err = app.CheckProviderUpdates()
	if err != nil {
		t.Fatal(err)
	}
	if claude := providerUpdateStatus(t, statuses, string(provider.Claude)); claude.SignInRequired || claude.CheckError != "" {
		t.Fatalf("claude after a sign-in = %+v, want a check that answered", claude)
	}
	mu.Lock()
	defer mu.Unlock()
	if doctors != 1 {
		t.Fatalf("ran %d doctors after the sign-in, want 1", doctors)
	}
}
