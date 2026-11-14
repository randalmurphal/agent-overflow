package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/accountswitch"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/store"
)

// autoSwitchRig is one provider's saved accounts, all in the auto-switch
// pool by default, with the last one selected, plus what the app reports
// about them.
type autoSwitchRig struct {
	app       *App
	provider  string
	auditPath string
	probes    atomic.Int32

	mu      sync.Mutex
	reports []accountswitch.Report
}

func newAutoSwitchRig(t *testing.T, app *App, providerName string, ids ...string) *autoSwitchRig {
	t.Helper()
	rig := &autoSwitchRig{app: app, provider: providerName, auditPath: filepath.Join(t.TempDir(), "audit.log")}
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials := newTestProviderCredentials(t, t.TempDir())
	var activeCredential []byte
	for _, id := range ids {
		credential := []byte(`{"claudeAiOauth":{"accessToken":"` + id + `"}}`)
		if providerName == string(provider.Codex) {
			credential = []byte(`{"tokens":{"access_token":"` + id + `"}}`)
		}
		if err := credentials.WriteAccountCredential(providerName, id, credential); err != nil {
			t.Fatal(err)
		}
		if _, err := accounts.UpsertAndActivate(provideraccounts.Account{
			ID: id, Provider: providerName, Email: id + "@example.com",
		}); err != nil {
			t.Fatal(err)
		}
		activeCredential = credential
	}
	if err := credentials.Activate(providerName, "", ids[len(ids)-1]); err != nil {
		t.Fatal(err)
	}
	app.providerAccounts = newProviderAccountManager(app)
	if err := app.providerAccounts.Attach(accounts, credentials, rig.auditPath); err != nil {
		t.Fatal(err)
	}
	app.providerAccounts.BlessCredentialForTest(providerName, activeCredential)
	app.switchedAccountProbeFn = func() error {
		rig.probes.Add(1)
		return nil
	}
	app.testEmitHook = func(name string, data any) {
		if name != string(eventchan.ProviderAccountAutoSwitch) {
			return
		}
		rig.mu.Lock()
		rig.reports = append(rig.reports, data.(accountswitch.Report))
		rig.mu.Unlock()
	}
	return rig
}

func (r *autoSwitchRig) setPool(t *testing.T, id string, inPool bool) {
	t.Helper()
	if err := r.app.providerAccounts.SetProviderAccountAutoSwitch(r.provider, id, inPool); err != nil {
		t.Fatal(err)
	}
}

func (r *autoSwitchRig) settings(t *testing.T, values map[string]any) {
	t.Helper()
	if _, err := r.app.settings.Update(values); err != nil {
		t.Fatal(err)
	}
}

// start runs the coordinator's worker for the rest of the test.
func (r *autoSwitchRig) start(t *testing.T) {
	t.Helper()
	r.app.startAccountSwitch()
	t.Cleanup(r.app.accountSwitch().Stop)
}

func (r *autoSwitchRig) selected() string {
	return r.app.captureProviderAccountSelection(r.provider).AccountID
}

func (r *autoSwitchRig) reported() []accountswitch.Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]accountswitch.Report(nil), r.reports...)
}

// reading publishes a complete usage reading of one account, taken now.
func (r *autoSwitchRig) reading(accountID string, limits ...provider.RateLimitEntry) {
	r.app.emit(eventchan.ProviderUsage, provider.UsageEvent{
		Action: "rate_limits",
		RateLimits: &provider.RateLimitsSnapshot{
			Provider: r.provider, AccountID: accountID, Limits: limits,
			UpdatedAt: time.Now().UnixMilli(), Complete: true,
		},
	})
}

func usageLimit(id string, percent float64, resetsIn time.Duration) provider.RateLimitEntry {
	return provider.RateLimitEntry{
		LimitID: id, UsedPercent: percent, WindowMins: 300,
		ResetsAt: time.Now().Add(resetsIn).Unix(),
	}
}

// holdsFor fails when cond turns true within a short settle window: the
// worker has no idle signal, so an absence is checked over time.
func holdsFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("%s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// putAccountSession registers a live session of thread on accountID at the
// provider's current generation.
func putAccountSession(t *testing.T, app *App, thread store.Thread, accountID string) {
	t.Helper()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: thread.Provider, Token: "token-" + thread.ID,
		CredentialGeneration: app.providerAccounts.Generation(thread.Provider),
		CredentialAccountID:  accountID,
		LaunchOptions:        provider.SessionOptions{Model: thread.Model},
		Liveness:             newSessionLiveness(time.Now()),
	})
}

func autoSwitchThread(id, providerName, model string) store.Thread {
	thread := testThread(id)
	thread.Provider = providerName
	thread.Model = model
	return thread
}

func usageLimitRefusal(threadID string) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: threadID, Content: "usage limit reached",
		Failure:   &provider.FailureMeta{Reason: provider.FailureReasonUsageLimit},
		Timestamp: time.Now(),
	}
}

func queuedMessages(app *App, threadID string) []string {
	var out []string
	for _, item := range app.queueSnapshotForThread(threadID) {
		out = append(out, item.Message)
	}
	return out
}

// A reading puts the selected Claude account past the threshold while a turn
// runs: the selection moves, the running session follows it, and the
// person is told in the toast event and in the running thread.
func TestAutoSwitchClaudeReadingPastThresholdMovesRunningTurns(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
	thread := autoSwitchThread("claude-running", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, thread, "second")
	openClaudeTurn(t, app, thread.ID)
	rig.start(t)

	rig.reading("first", usageLimit("session", 10, time.Hour))
	rig.reading("second", usageLimit("session", 96, time.Hour))

	waitForCondition(t, "the auto-switch report", func() bool { return len(rig.reported()) == 1 })
	report := rig.reported()[0]
	if report.FromAccountID != "second" || report.ToAccountID != "first" || report.Reason != accountswitch.ReasonThreshold || report.Error != "" {
		t.Fatalf("report = %+v, want second -> first for the threshold", report)
	}
	if got := rig.selected(); got != "first" {
		t.Fatalf("selected account = %q, want first", got)
	}
	if sess, _ := app.sessionManager().get(thread.ID); sess.CredentialAccountID != "first" {
		t.Fatalf("running session account = %q, want first", sess.CredentialAccountID)
	}
	waitForCondition(t, "the switch notice", func() bool {
		return len(timelineRows(t, app.store, thread.ID, "notification", noticeKindAccountAutoSwitch)) > 0
	})
	if rows := timelineRows(t, app.store, thread.ID, "notification", noticeKindAccountAutoSwitch); len(rows) != 1 || !strings.Contains(rows[0], "96%") {
		t.Fatalf("switch notices = %q, want one naming the 96%% limit", rows)
	}
	waitForCondition(t, "the post-switch probe", func() bool { return rig.probes.Load() == 1 })
}

// Auto-switch acts only while the selected account is in the pool and the
// pool has another account.
func TestAutoSwitchReadingIgnoredOutsideAnActivePool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		unticked string
	}{
		{name: "only the selected account in the pool", unticked: "first"},
		{name: "selected account out of the pool", unticked: "second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			app.ensureTriageRouter()
			rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
			rig.setPool(t, tc.unticked, false)
			thread := autoSwitchThread("claude-running", string(provider.Claude), "claude-opus-5")
			putAccountSession(t, app, thread, "second")
			openClaudeTurn(t, app, thread.ID)
			rig.start(t)

			rig.reading("first", usageLimit("session", 10, time.Hour))
			rig.reading("second", usageLimit("session", 96, time.Hour))

			holdsFor(t, "auto-switch acted outside an active pool", func() bool {
				return len(rig.reported()) == 0 && rig.selected() == "second"
			})
			if rows := timelineRows(t, app.store, thread.ID, "notification", noticeKindAccountAutoSwitch); len(rows) != 0 {
				t.Fatalf("switch notices = %q, want none", rows)
			}
		})
	}
}

// A scoped limit matters only to a thread of that model: a Fable send moves
// off an account with no Fable usage left, an Opus send stays.
func TestAutoSwitchBeforeSendWeighsScopedLimitsByModel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		model string
		want  string
	}{
		{model: "claude-fable-5-2", want: "first"},
		{model: "claude-opus-5", want: "second"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			app.ensureTriageRouter()
			rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
			rig.reading("first", usageLimit("weekly_all", 5, 72*time.Hour))
			rig.reading("second",
				usageLimit("weekly_scoped:fable", 100, 72*time.Hour),
				usageLimit("weekly_all", 40, 72*time.Hour))
			thread := autoSwitchThread("idle", string(provider.Claude), tc.model)
			putAccountSession(t, app, thread, "second")

			if err := app.ensureProviderAccountReadyForSendLocked(thread); err != nil {
				t.Fatalf("send gate: %v", err)
			}
			if got := rig.selected(); got != tc.want {
				t.Fatalf("selected account = %q, want %q", got, tc.want)
			}
			if sess, _ := app.sessionManager().get(thread.ID); sess.CredentialAccountID != tc.want {
				t.Fatalf("session account = %q, want %q before dispatch", sess.CredentialAccountID, tc.want)
			}
		})
	}
}

// A message into a thread's own running turn (a steer) never switches.
func TestAutoSwitchNeverSwitchesOnASteer(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
	rig.reading("first", usageLimit("session", 5, time.Hour))
	rig.reading("second", usageLimit("session", 100, time.Hour))
	thread := autoSwitchThread("steering", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, thread, "second")
	openClaudeTurn(t, app, thread.ID)

	if err := app.ensureProviderAccountReadyForSendLocked(thread); err != nil {
		t.Fatalf("send gate: %v", err)
	}
	if got := rig.selected(); got != "second" || len(rig.reported()) != 0 {
		t.Fatalf("selected = %q reports = %+v, want no switch on a steer", got, rig.reported())
	}
}

// A switch cannot reach a running Codex turn, so a reading leaves the
// selection alone; the next send from an idle Codex thread switches and its
// session reconnects on the new account.
func TestAutoSwitchCodexWaitsForTheNextSend(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	rig := newAutoSwitchRig(t, app, string(provider.Codex), "first", "second")
	// The reading requested after the switch runs a Codex app-server probe.
	writeCodexAppServerMock(t, app)
	running := autoSwitchThread("codex-running", string(provider.Codex), "gpt-5.4")
	putAccountSession(t, app, running, "second")
	openCodexTurn(t, app, running.ID)
	idle := autoSwitchThread("codex-idle", string(provider.Codex), "gpt-5.4")
	putAccountSession(t, app, idle, "second")
	app.stopSessionFn = func(threadID string) error {
		app.sessionManager().take(threadID)
		return nil
	}
	app.startSessionFn = func(threadID string) error {
		selection := app.captureProviderAccountSelection(string(provider.Codex))
		app.sessionManager().put(threadID, session{
			Provider: string(provider.Codex), Token: "new-process",
			CredentialGeneration: selection.Generation, CredentialAccountID: selection.AccountID,
			Liveness: newSessionLiveness(time.Now()),
		})
		return nil
	}
	rig.start(t)

	rig.reading("first", usageLimit("codex", 5, time.Hour))
	rig.reading("second", usageLimit("codex", 100, time.Hour))
	holdsFor(t, "a reading switched Codex under a running turn", func() bool {
		return len(rig.reported()) == 0 && rig.selected() == "second"
	})

	if err := app.ensureProviderAccountReadyForSendLocked(idle); err != nil {
		t.Fatalf("send gate: %v", err)
	}
	if got := rig.selected(); got != "first" {
		t.Fatalf("selected account = %q, want first", got)
	}
	if sess, _ := app.sessionManager().get(idle.ID); sess.Token != "new-process" || sess.CredentialAccountID != "first" {
		t.Fatalf("idle Codex session = %+v, want reconnected on first", sess)
	}
	if reports := rig.reported(); len(reports) != 1 || reports[0].Reason != accountswitch.ReasonExhausted {
		t.Fatalf("reports = %+v, want one exhausted switch", reports)
	}
}

// A selection the person made holds while it serves; after a forced switch
// the selection is the app's, and an idle send returns to the first account
// in the pool order once it is clear. Both kinds of switch run the
// post-switch probe.
func TestAutoSwitchPinnedSelectionAndOrderReturn(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "a", "b", "c")
	rig.settings(t, map[string]any{"accountAutoSwitchStrategy": "account-order"})
	if _, err := app.SwitchProviderAccount(string(provider.Claude), "b"); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "the manual switch probe", func() bool { return rig.probes.Load() == 1 })
	for _, id := range []string{"a", "b", "c"} {
		rig.reading(id, usageLimit("session", 5, time.Hour))
	}
	thread := autoSwitchThread("idle", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, thread, "b")

	send := func() {
		t.Helper()
		if err := app.ensureProviderAccountReadyForSendLocked(thread); err != nil {
			t.Fatalf("send gate: %v", err)
		}
	}
	send()
	if got := rig.selected(); got != "b" || len(rig.reported()) != 0 {
		t.Fatalf("selected = %q reports = %+v, want the manual selection kept", got, rig.reported())
	}

	rig.reading("a", usageLimit("session", 100, time.Hour))
	rig.reading("b", usageLimit("session", 100, time.Hour))
	send()
	if got := rig.selected(); got != "c" {
		t.Fatalf("selected after forced switch = %q, want c", got)
	}

	rig.reading("a", usageLimit("session", 5, 5*time.Hour))
	send()
	if got := rig.selected(); got != "a" {
		t.Fatalf("selected after idle send = %q, want a, first in the pool order", got)
	}
	reports := rig.reported()
	if len(reports) != 2 || reports[0].Reason != accountswitch.ReasonExhausted || reports[1].Reason != accountswitch.ReasonOrder {
		t.Fatalf("reports = %+v, want exhausted then order", reports)
	}
	waitForCondition(t, "the automatic switch probes", func() bool { return rig.probes.Load() == 3 })
}

// A target that needs a sign-in is skipped for the next one; a switch that
// fails otherwise is reported once and audited, and the send goes on.
func TestAutoSwitchSkipsSignInAndReportsFailures(t *testing.T) {
	t.Parallel()
	t.Run("needs sign-in", func(t *testing.T) {
		t.Parallel()
		app := newTestAppWithStore(t)
		app.ensureTriageRouter()
		rig := newAutoSwitchRig(t, app, string(provider.Claude), "a", "b", "c")
		rig.settings(t, map[string]any{"accountAutoSwitchStrategy": "account-order"})
		slot, err := providerCredentialsForTest(t, app).AccountCredentialPath(string(provider.Claude), "a")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(slot); err != nil {
			t.Fatal(err)
		}
		rig.reading("a", usageLimit("session", 5, time.Hour))
		rig.reading("b", usageLimit("session", 5, time.Hour))
		rig.reading("c", usageLimit("session", 100, time.Hour))
		thread := autoSwitchThread("idle", string(provider.Claude), "claude-opus-5")
		putAccountSession(t, app, thread, "c")

		if err := app.ensureProviderAccountReadyForSendLocked(thread); err != nil {
			t.Fatalf("send gate: %v", err)
		}
		if got := rig.selected(); got != "b" {
			t.Fatalf("selected = %q, want b after a needed a sign-in", got)
		}
		if reports := rig.reported(); len(reports) != 1 || reports[0].ToAccountID != "b" || reports[0].Error != "" {
			t.Fatalf("reports = %+v, want one switch to b", reports)
		}
	})
	t.Run("switch error", func(t *testing.T) {
		t.Parallel()
		app := newTestAppWithStore(t)
		app.ensureTriageRouter()
		rig := newAutoSwitchRig(t, app, string(provider.Claude), "a", "c")
		slot, err := providerCredentialsForTest(t, app).AccountCredentialPath(string(provider.Claude), "a")
		if err != nil {
			t.Fatal(err)
		}
		// A directory where the credential should be is a read error that is
		// not a missing credential.
		if err := os.Remove(slot); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(slot, 0o700); err != nil {
			t.Fatal(err)
		}
		rig.reading("a", usageLimit("session", 5, time.Hour))
		rig.reading("c", usageLimit("session", 100, time.Hour))
		thread := autoSwitchThread("idle", string(provider.Claude), "claude-opus-5")
		putAccountSession(t, app, thread, "c")

		for range 2 {
			if err := app.ensureProviderAccountReadyForSendLocked(thread); err != nil {
				t.Fatalf("send gate: %v, want the send to go on", err)
			}
		}
		if got := rig.selected(); got != "c" {
			t.Fatalf("selected = %q, want c kept", got)
		}
		reports := rig.reported()
		if len(reports) != 1 || reports[0].ToAccountID != "a" || reports[0].Error == "" {
			t.Fatalf("reports = %+v, want one failed switch to a", reports)
		}
		audit, err := os.ReadFile(rig.auditPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(audit), "auto-switch of claude account from c to a failed") {
			t.Fatalf("audit log = %q, want the failed switch", audit)
		}
	})
}

// A request in flight on the previous account when a reading moved the
// selection can still be refused there. The refusal is reported with the
// session's new account but can belong to either, so it marks neither and
// the new account stays in use.
func TestAutoSwitchRefusalJustAfterASwitchKeepsTheNewAccount(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "b", "c", "a")
	rig.settings(t, map[string]any{"accountAutoSwitchStrategy": "account-order"})
	thread := autoSwitchThread("running", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, thread, "a")
	openClaudeTurn(t, app, thread.ID)
	rig.start(t)

	rig.reading("b", usageLimit("session", 5, time.Hour))
	rig.reading("c", usageLimit("session", 5, time.Hour))
	rig.reading("a", usageLimit("session", 100, time.Hour))
	waitForCondition(t, "the switch report", func() bool { return len(rig.reported()) == 1 })
	if got := rig.selected(); got != "b" {
		t.Fatalf("selected = %q, want b", got)
	}
	if sess, _ := app.sessionManager().get(thread.ID); sess.CredentialAccountID != "b" {
		t.Fatalf("running session account = %q, want b", sess.CredentialAccountID)
	}

	app.dispatchTurnObservers(thread.ID, usageLimitRefusal(thread.ID))
	holdsFor(t, "the refusal moved the selection off b", func() bool {
		return len(rig.reported()) == 1 && rig.selected() == "b"
	})
	idle := autoSwitchThread("idle", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, idle, "b")
	if err := app.ensureProviderAccountReadyForSendLocked(idle); err != nil {
		t.Fatal(err)
	}
	if got := rig.selected(); got != "b" {
		t.Fatalf("selected = %q after a send, want b: the refusal did not mark it", got)
	}
}

// A failure to reconcile the current account during an automatic switch is
// a failed switch, not a target that needs a sign-in, even when it wraps
// the expired-login verdict.
func TestAutoSwitchReconcileFailureIsAFailedSwitch(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("check current claude account before switch: %w", provideraccountapp.ErrClaudeLoginExpired)
	if provideraccountapp.IsNeedsLogin(wrapped) {
		t.Fatal("a reconcile verdict about the current account reads as the target needing a sign-in")
	}

	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "a", "c")
	rig.reading("a", usageLimit("session", 5, time.Hour))
	rig.reading("c", usageLimit("session", 100, time.Hour))
	canonical, err := providerCredentialsForTest(t, app).ActiveCredentialPath(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	// A directory where the canonical credential should be fails the
	// reconcile that every switch runs first.
	if err := os.Remove(canonical); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(canonical, 0o700); err != nil {
		t.Fatal(err)
	}
	needsLogin, err := app.accountSwitchDeps().Switch(string(provider.Claude), "a")
	if needsLogin || err == nil || !strings.Contains(err.Error(), "before switch") {
		t.Fatalf("Switch = needsLogin %v, err %v; want the reconcile failure", needsLogin, err)
	}
	app.accountSwitch().BeforeSend(string(provider.Claude), "idle", "claude-opus-5", false)
	reports := rig.reported()
	if len(reports) != 1 || reports[0].ToAccountID != "a" || !strings.Contains(reports[0].Error, "before switch") {
		t.Fatalf("reports = %+v, want one failed switch to a", reports)
	}
}

// The refusal observer passes the account the refused session was using,
// not the provider-wide selection.
func TestAutoSwitchRefusalMarksTheSessionAccount(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
	rig.reading("first", usageLimit("session", 5, time.Hour))
	rig.reading("second", usageLimit("session", 5, time.Hour))
	// The session still runs on first while second is selected.
	thread := autoSwitchThread("stale-session", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, thread, "first")
	rig.start(t)

	app.dispatchTurnObservers(thread.ID, usageLimitRefusal(thread.ID))
	holdsFor(t, "a refusal on first moved the selection off second", func() bool {
		return len(rig.reported()) == 0 && rig.selected() == "second"
	})
	// first now holds the mark. With the selection the app's own, a send
	// that would return to first stays on second.
	rig.settings(t, map[string]any{"accountAutoSwitchStrategy": "account-order"})
	for _, id := range []string{"first", "second"} {
		if _, err := app.switchProviderAccount(string(provider.Claude), id, true); err != nil {
			t.Fatal(err)
		}
	}
	idle := autoSwitchThread("idle", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, idle, "second")
	if err := app.ensureProviderAccountReadyForSendLocked(idle); err != nil {
		t.Fatal(err)
	}
	if got := rig.selected(); got != "second" {
		t.Fatalf("selected = %q, want second: first carries the refusal mark", got)
	}
}

// Removing an account forgets its refusal marks, so a re-added account
// starts clear.
func TestAutoSwitchAccountsChangedForgetsRemovedAccounts(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "a", "b", "c")
	rig.reading("a", usageLimit("session", 5, time.Hour))
	app.accountSwitch().TurnRefused(string(provider.Claude), "elsewhere", "claude-opus-5", "a")

	credential, err := providerCredentialsForTest(t, app).ReadCredential(string(provider.Claude), "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.providerAccounts.RemoveProviderAccount(string(provider.Claude), "a"); err != nil {
		t.Fatal(err)
	}
	if err := providerCredentialsForTest(t, app).WriteAccountCredential(string(provider.Claude), "a", credential); err != nil {
		t.Fatal(err)
	}
	if _, err := app.providerAccounts.MetadataStoreForTest().Add(provideraccounts.Account{
		ID: "a", Provider: string(provider.Claude), Email: "a@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	rig.reading("a", usageLimit("session", 5, time.Hour))
	rig.reading("b", usageLimit("session", 100, time.Hour))
	rig.reading("c", usageLimit("session", 100, time.Hour))

	idle := autoSwitchThread("idle", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, idle, "c")
	if err := app.ensureProviderAccountReadyForSendLocked(idle); err != nil {
		t.Fatal(err)
	}
	if got := rig.selected(); got != "a" {
		t.Fatalf("selected = %q, want a: its refusal mark went with the account", got)
	}
}
