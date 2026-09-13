package app

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/providerstatus"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// recordingNotificationSender is the whole platform layer, replaced. Every
// test in this file installs one, so no test in this package can reach a
// notification daemon even if the fixture it builds on grows a real sender.
type recordingNotificationSender struct {
	mu    sync.Mutex
	sends []notify.Send
}

func (r *recordingNotificationSender) send(payload notify.Send) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, payload)
	return nil
}

func (r *recordingNotificationSender) snapshot() []notify.Send {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Send(nil), r.sends...)
}

const (
	mappingThreadID = "thread-notify"
	mappingTitle    = "Rewrite the parser"
)

// newNotificationMappingApp is the fixture for the tap: the isolated
// store-backed App, one titled thread, and a recorder in place of the
// platform.
func newNotificationMappingApp(t *testing.T) (*App, *recordingNotificationSender) {
	t.Helper()
	app := newTestAppWithStore(t)
	recorder := &recordingNotificationSender{}
	app.osNotifications = recorder

	thread := testThread(mappingThreadID)
	thread.Title = mappingTitle
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.storeIdentity.Store(&store.Identity{BackendID: "backend-under-test"})
	return app, recorder
}

// settled emits, waits for the notification queue to finish, and answers what
// the platform layer was asked to do. The wait is the queue's own, not a
// sleep: a mapped moment is finished off the emitting goroutine by contract.
func settled(t *testing.T, a *App, recorder *recordingNotificationSender, emissions ...func()) []notify.Send {
	t.Helper()
	for _, emission := range emissions {
		emission()
	}
	a.notifications.queue.Wait()
	return recorder.snapshot()
}

func turnCompleted(a *App, failed bool) func() {
	return func() {
		event := triage.TurnCompletedEvent{
			ThreadID:         mappingThreadID,
			TurnID:           "turn-1",
			CountsAsActivity: true,
			StopReason:       "end_turn",
			TokenUsage:       json.RawMessage(`{"input":1200,"output":840}`),
		}
		if failed {
			event.ErrorMessage = "the provider returned 529 while streaming the answer"
		}
		a.emit(eventchan.ProviderTurnCompleted, event)
	}
}

func approvalRequested(a *App) func() {
	return func() {
		a.emit(eventchan.ProviderApproval, provider.ApprovalEvent{
			Action: "request",
			Request: &provider.ApprovalRequest{
				RequestID:   "req-1",
				ThreadID:    mappingThreadID,
				ToolName:    "Bash",
				Description: "rm -rf ./build && pnpm run build",
				Input:       json.RawMessage(`{"command":"rm -rf ./build"}`),
			},
		})
	}
}

// TestTurnCompletionNotifiesWithTheThreadsOwnTitle walks the whole path for
// the most common moment: a wire event on the funnel, a title read from
// SQLite off the emitting goroutine, and one send.
func TestTurnCompletionNotifiesWithTheThreadsOwnTitle(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	sends := settled(t, app, recorder, turnCompleted(app, false))
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want 1: %#v", len(sends), sends)
	}
	send := sends[0]
	if send.Kind != notify.KindTurnComplete {
		t.Fatalf("kind = %q", send.Kind)
	}
	if send.Title != mappingTitle {
		t.Fatalf("title = %q, want the thread's title %q", send.Title, mappingTitle)
	}
	if send.Body != "Completed" {
		t.Fatalf("body = %q", send.Body)
	}
	if send.Target.ThreadID != mappingThreadID {
		t.Fatalf("target = %#v, want the thread", send.Target)
	}
	if send.Target.BackendID != "backend-under-test" {
		t.Fatalf("backend id = %q, want this backend's", send.Target.BackendID)
	}
}

// TestASubagentRoundDoesNotInterruptAnyone: a Task tool's own turn completes
// while the top-level agent keeps working, and the user is not told the
// thread is done.
func TestASubagentRoundDoesNotInterruptAnyone(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	sends := settled(t, app, recorder, func() {
		app.emit(eventchan.ProviderTurnCompleted, triage.TurnCompletedEvent{
			ThreadID: mappingThreadID, TurnID: "turn-1", CountsAsActivity: false,
		})
	})
	if len(sends) != 0 {
		t.Fatalf("a subagent round notified: %#v", sends)
	}
}

// TestResumingAThreadWithdrawsItsRestNotification is the retraction contract
// at the app level: the completion the user never looked at is taken back
// when the thread starts working again, by the id that presented it.
func TestResumingAThreadWithdrawsItsRestNotification(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	sends := settled(t, app, recorder,
		turnCompleted(app, false),
		func() {
			app.emit(eventchan.ProviderTurnStarted, triage.TurnStartedEvent{
				ThreadID: mappingThreadID, TurnID: "turn-2",
			})
		},
	)
	if len(sends) != 2 {
		t.Fatalf("sends = %d, want a presentation and its retraction: %#v", len(sends), sends)
	}
	if !sends[1].Retract {
		t.Fatalf("second send is not a retraction: %#v", sends[1])
	}
	if sends[1].ID != sends[0].ID {
		t.Fatalf("retraction id = %q, want the presented %q", sends[1].ID, sends[0].ID)
	}
	if sends[1].Target != (notify.Target{}) {
		t.Fatalf("retraction carries a route: %#v", sends[1].Target)
	}
}

// TestAFailedTurnAndADeadProviderReplaceEachOtherAndSayNothingSpecific
// covers the two error moments together, because they share one identifier
// on purpose — a thread holds one rest notification — and both are the ones
// with provider prose in the event to leave behind.
func TestAFailedTurnAndADeadProviderReplaceEachOtherAndSayNothingSpecific(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	sends := settled(t, app, recorder,
		turnCompleted(app, true),
		func() {
			app.emit(eventchan.ProviderSessionDied, triage.SessionDiedEvent{
				ThreadID:   mappingThreadID,
				Reason:     "process exited",
				ExitCode:   137,
				StderrTail: "panic: cannot open /home/dev/.config/secret-token.json",
			})
		},
	)
	if len(sends) != 2 {
		t.Fatalf("sends = %d, want 2: %#v", len(sends), sends)
	}
	if sends[0].ID != sends[1].ID {
		t.Fatalf("the two rest moments took different ids: %q / %q", sends[0].ID, sends[1].ID)
	}
	for _, send := range sends {
		if send.Kind != notify.KindError {
			t.Fatalf("kind = %q, want %q", send.Kind, notify.KindError)
		}
		if send.Title != mappingTitle {
			t.Fatalf("title = %q", send.Title)
		}
		if strings.Contains(send.Body, "529") || strings.Contains(send.Body, "secret-token") ||
			strings.Contains(send.Body, "137") {
			t.Fatalf("body carried the provider's own prose: %q", send.Body)
		}
	}
}

// TestAnApprovalIsPresentedThenWithdrawnWhenItIsAnswered covers the moment
// with the most branches on the wire: the request carries the whole
// ApprovalRequest, and the resolve carries only its id.
func TestAnApprovalIsPresentedThenWithdrawnWhenItIsAnswered(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	sends := settled(t, app, recorder,
		approvalRequested(app),
		func() {
			app.emit(eventchan.ProviderApproval, provider.ApprovalEvent{
				Action: "resolve", ThreadID: mappingThreadID,
				RequestID: "req-1", Decision: "approved",
			})
		},
	)
	if len(sends) != 2 {
		t.Fatalf("sends = %d, want a prompt and its withdrawal: %#v", len(sends), sends)
	}
	if sends[0].Kind != notify.KindApprovalNeeded || sends[0].Body != "Pending approval: Bash" {
		t.Fatalf("prompt = %#v", sends[0])
	}
	if strings.Contains(sends[0].Body, "rm -rf") {
		t.Fatalf("the prompt carried the command: %q", sends[0].Body)
	}
	if !sends[1].Retract || sends[1].ID != sends[0].ID {
		t.Fatalf("withdrawal = %#v, want a retraction of %q", sends[1], sends[0].ID)
	}
}

// TestAFailedApprovalStillWithdrawsThePrompt: an approval that is lost with
// its session is answered as far as the user is concerned, and a
// notification pointing at a prompt that no longer exists is the stale alert
// retraction exists to prevent.
func TestAFailedApprovalStillWithdrawsThePrompt(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	sends := settled(t, app, recorder,
		approvalRequested(app),
		func() {
			app.emit(eventchan.ProviderApproval, provider.ApprovalEvent{
				Action: "fail", ThreadID: mappingThreadID,
				RequestID: "req-1", Decision: "lost",
			})
		},
	)
	if len(sends) != 2 || !sends[1].Retract {
		t.Fatalf("sends = %#v, want the prompt withdrawn", sends)
	}
}

// TestProviderSignOutIsAnEdgeNotALevel: `provider:status` re-emits
// `unauthenticated` every time anything asks for provider statuses, so only
// the transition may notify — otherwise opening the provider picker would
// raise the same alert again.
func TestProviderSignOutIsAnEdgeNotALevel(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	signedOut := func() {
		app.emit(eventchan.ProviderStatus, providerstatus.Event{
			Provider: "claude", Status: "unauthenticated",
			Message: "Run `claude login` to authenticate",
		})
	}
	sends := settled(t, app, recorder, signedOut, signedOut, signedOut)
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want 1 for three level re-emissions: %#v", len(sends), sends)
	}
	send := sends[0]
	if send.Kind != notify.KindProviderSignedOut {
		t.Fatalf("kind = %q", send.Kind)
	}
	if send.Title != "Claude signed out" || send.Body != "Sign in again to keep running turns." {
		t.Fatalf("copy = %q / %q", send.Title, send.Body)
	}
	if send.Target.Kind != "none" {
		t.Fatalf("target = %#v, want no route", send.Target)
	}
	if strings.Contains(send.Body, "claude login") {
		t.Fatalf("body carried the status message: %q", send.Body)
	}

	// And signing back in takes it away.
	sends = settled(t, app, recorder, func() {
		app.emit(eventchan.ProviderLogin, provideraccountapp.LoginState{
			Provider: "claude", Phase: provideraccountapp.LoginPhaseSucceeded,
		})
	})
	if len(sends) != 2 || !sends[1].Retract || sends[1].ID != send.ID {
		t.Fatalf("sends = %#v, want the alert withdrawn", sends)
	}
}

// TestAnOrdinarySignInRetractsNothing: the first successful sign-in of a
// session, with no alert standing, must not send a retraction for a
// notification that was never presented.
func TestAnOrdinarySignInRetractsNothing(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	sends := settled(t, app, recorder, func() {
		app.emit(eventchan.ProviderLogin, provideraccountapp.LoginState{
			Provider: "codex", Phase: provideraccountapp.LoginPhaseSucceeded,
		})
	})
	if len(sends) != 0 {
		t.Fatalf("an ordinary sign-in sent something: %#v", sends)
	}
}

// TestAnUnfinishedLoginIsNotASignIn: only the succeeded phase is an edge.
func TestAnUnfinishedLoginIsNotASignIn(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	sends := settled(t, app, recorder,
		func() {
			app.emit(eventchan.ProviderStatus, providerstatus.Event{
				Provider: "claude", Status: "unauthenticated",
			})
		},
		func() {
			app.emit(eventchan.ProviderLogin, provideraccountapp.LoginState{
				Provider: "claude", Phase: provideraccountapp.LoginPhaseAwaitingBrowser,
			})
		},
	)
	if len(sends) != 1 || sends[0].Retract {
		t.Fatalf("sends = %#v, want only the signed-out alert", sends)
	}
}

// TestAReadyStatusIsNotASignIn: `provider:status` is a level for every
// status, so a `ready` re-emission must not be read as the user having just
// signed in.
func TestAReadyStatusIsNotASignIn(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	sends := settled(t, app, recorder,
		func() {
			app.emit(eventchan.ProviderStatus, providerstatus.Event{
				Provider: "claude", Status: "unauthenticated",
			})
		},
		func() {
			app.emit(eventchan.ProviderStatus, providerstatus.Event{
				Provider: "claude", Status: "ready", Version: "claude 2.1.0",
			})
		},
	)
	if len(sends) != 1 || sends[0].Retract {
		t.Fatalf("sends = %#v, want only the signed-out alert", sends)
	}
}

// TestPreferenceSilencesOnlyTheKindItNames is the per-kind gate: one toggle
// off must not silence the other three.
func TestPreferenceSilencesOnlyTheKindItNames(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	if _, err := app.settings.BackendScreen().Update(map[string]any{
		"notifyTurnComplete": false,
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	sends := settled(t, app, recorder, turnCompleted(app, false), approvalRequested(app))
	if len(sends) != 1 {
		t.Fatalf("sends = %#v, want only the approval", sends)
	}
	if sends[0].Kind != notify.KindApprovalNeeded {
		t.Fatalf("kind = %q, want the approval to survive", sends[0].Kind)
	}
}

// The two kinds that reach no event tap — a workflow item waiting on a
// person, and the launcher's "update didn't apply" notice — each have a
// toggle of their own now, and it silences that kind and nothing else. They
// are driven through notifyOS directly because that is how their senders
// reach it: neither is mapped off the event funnel.
func TestWorkflowAttentionAndAppUpdateHaveTogglesOfTheirOwn(t *testing.T) {
	for _, tc := range []struct {
		key      string
		silenced notify.Kind
		survives notify.Kind
	}{
		{"notifyWorkflowAttention", notify.KindWorkflowAttention, notify.KindAppUpdate},
		{"notifyAppUpdate", notify.KindAppUpdate, notify.KindWorkflowAttention},
	} {
		t.Run(tc.key, func(t *testing.T) {
			app, recorder := newNotificationMappingApp(t)
			if _, err := app.settings.BackendScreen().Update(map[string]any{
				tc.key: false,
			}); err != nil {
				t.Fatalf("update settings: %v", err)
			}
			silenced := notify.Send{ID: "a", Kind: tc.silenced, Title: "t", Target: notify.Target{Kind: "none"}}
			err := app.notifyOS(silenced)
			var notificationErr *NotificationError
			if !errors.As(err, &notificationErr) || notificationErr.Code != NotificationSuppressed {
				t.Fatalf("notifyOS(%s) = %v, want suppressed", tc.silenced, err)
			}
			survives := notify.Send{ID: "b", Kind: tc.survives, Title: "t", Target: notify.Target{Kind: "none"}}
			if err := app.notifyOS(survives); err != nil {
				t.Fatalf("notifyOS(%s) = %v, want the other kind untouched", tc.survives, err)
			}
			if sends := recorder.snapshot(); len(sends) != 1 || sends[0].Kind != tc.survives {
				t.Fatalf("presenter saw %#v, want only %s", sends, tc.survives)
			}
		})
	}
}

// TestTheMasterSwitchSilencesEveryKind, every one of the six: "off" on this
// screen has to mean off, whatever the per-kind toggles say.
func TestTheMasterSwitchSilencesEveryKind(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	if _, err := app.settings.BackendScreen().Update(map[string]any{
		"notificationsEnabled": false,
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	sends := settled(t, app, recorder,
		turnCompleted(app, false),
		approvalRequested(app),
		turnCompleted(app, true),
		func() {
			app.emit(eventchan.ProviderStatus, providerstatus.Event{
				Provider: "claude", Status: "unauthenticated",
			})
		},
	)
	if len(sends) != 0 {
		t.Fatalf("the master switch let %d sends through: %#v", len(sends), sends)
	}
	for _, kind := range []notify.Kind{notify.KindWorkflowAttention, notify.KindAppUpdate} {
		send := notify.Send{ID: "x", Kind: kind, Title: "t", Target: notify.Target{Kind: "none"}}
		err := app.notifyOS(send)
		var notificationErr *NotificationError
		if !errors.As(err, &notificationErr) ||
			notificationErr.Code != NotificationSuppressed {
			t.Fatalf("notifyOS(%s) = %v, want suppressed", kind, err)
		}
	}
}

// TestSuppressionIsTypedNotSilent: the queue drops a suppressed send without
// logging, but a direct caller — the workflow attention sender, the update
// notice — gets a code it can distinguish from a broken daemon.
func TestSuppressionIsTypedNotSilent(t *testing.T) {
	app, _ := newNotificationMappingApp(t)
	if _, err := app.settings.BackendScreen().Update(map[string]any{
		"notifyError": false,
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	err := app.notifyOS(notify.Send{
		ID: "thread:x", Kind: notify.KindError, Title: "t",
		Target: notify.Target{Kind: "none"},
	})
	var notificationErr *NotificationError
	if !errors.As(err, &notificationErr) ||
		notificationErr.Code != NotificationSuppressed {
		t.Fatalf("notifyOS = %v, want a suppressed notification error", err)
	}
	if !strings.Contains(err.Error(), "turned off") {
		t.Fatalf("suppressed error = %q, want user-facing text", err)
	}
}

// TestARetractionIsNeverSuppressed would be a notification the user cannot
// clear: turning a kind off between the send and the retraction must not
// strand what is already on screen.
func TestARetractionIsNeverSuppressed(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	sends := settled(t, app, recorder, turnCompleted(app, false))
	if len(sends) != 1 {
		t.Fatalf("sends = %#v, want the completion", sends)
	}
	if _, err := app.settings.BackendScreen().Update(map[string]any{
		"notifyTurnComplete": false,
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	sends = settled(t, app, recorder, func() {
		app.emit(eventchan.ProviderTurnStarted, triage.TurnStartedEvent{
			ThreadID: mappingThreadID, TurnID: "turn-2",
		})
	})
	if len(sends) != 2 || !sends[1].Retract {
		t.Fatalf("sends = %#v, want the retraction to survive the toggle", sends)
	}
}

// hiddenThreads writes the opt-in for threads the sidebar does not list onto
// the backend machine's own screen, which is the screen the gate reads.
func hiddenThreads(t *testing.T, app *App, on bool) {
	t.Helper()
	if _, err := app.settings.BackendScreen().Update(map[string]any{
		"notifyHiddenThreads": on,
	}); err != nil {
		t.Fatalf("update hidden-threads setting: %v", err)
	}
}

// TestAThreadWithNoRowIsNotInTheSidebar: a thread that is gone, or one that
// was never persisted (an agent's ephemeral question), is one the sidebar
// cannot show, so it is judged by the hidden-thread opt-in. With it on, the
// title read is still best-effort and the notification carries the generic
// heading.
func TestAThreadWithNoRowIsNotInTheSidebar(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	completed := func() {
		app.emit(eventchan.ProviderTurnCompleted, triage.TurnCompletedEvent{
			ThreadID: "thread-that-was-deleted", CountsAsActivity: true,
		})
	}
	if sends := settled(t, app, recorder, completed); len(sends) != 0 {
		t.Fatalf("a thread with no row notified by default: %#v", sends)
	}

	hiddenThreads(t, app, true)
	sends := settled(t, app, recorder, completed)
	if len(sends) != 1 {
		t.Fatalf("sends = %#v, want one once the screen opted in", sends)
	}
	if sends[0].Title != notify.UntitledThread {
		t.Fatalf("title = %q, want %q", sends[0].Title, notify.UntitledThread)
	}
	if !sends[0].HiddenThread {
		t.Fatalf("send = %#v, want it marked as a hidden thread's", sends[0])
	}
}

// TestAWorkflowThreadIsSilentUntilTheScreenOptsIn is the moment this opt-in
// exists for: a workflow phase thread is a real thread with a real title, the
// sidebar does not list it, and its turn completing, failing or asking for
// approval must not interrupt anyone who cannot click it. Every thread-scoped
// kind is covered by the one toggle, and the per-kind toggles still apply on
// top of it.
func TestAWorkflowThreadIsSilentUntilTheScreenOptsIn(t *testing.T) {
	const workflowThreadID = "thread-workflow-phase"
	app, recorder := newNotificationMappingApp(t)
	phase := testThread(workflowThreadID)
	phase.Title = "Phase 2: implement"
	phase.Mode = threadmode.ModeWorkflow
	if err := app.store.CreateThread(phase); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	moments := []func(){
		func() {
			app.emit(eventchan.ProviderTurnCompleted, triage.TurnCompletedEvent{
				ThreadID: workflowThreadID, TurnID: "turn-1", CountsAsActivity: true,
			})
		},
		func() {
			app.emit(eventchan.ProviderTurnCompleted, triage.TurnCompletedEvent{
				ThreadID: workflowThreadID, TurnID: "turn-2", CountsAsActivity: true,
				ErrorMessage: "the provider returned 529",
			})
		},
		func() {
			app.emit(eventchan.ProviderSessionDied, triage.SessionDiedEvent{ThreadID: workflowThreadID})
		},
		func() {
			app.emit(eventchan.ProviderApproval, provider.ApprovalEvent{
				Action: "request",
				Request: &provider.ApprovalRequest{
					RequestID: "req-1", ThreadID: workflowThreadID, ToolName: "Bash",
				},
			})
		},
	}
	if sends := settled(t, app, recorder, moments...); len(sends) != 0 {
		t.Fatalf("a workflow thread interrupted by default: %#v", sends)
	}

	// The ordinary thread beside it is untouched by the opt-in being off.
	if sends := settled(t, app, recorder, turnCompleted(app, false)); len(sends) != 1 {
		t.Fatalf("sends = %#v, want the visible thread's completion", sends)
	}

	hiddenThreads(t, app, true)
	sends := settled(t, app, recorder, moments...)
	if len(sends) != 5 {
		t.Fatalf("sends = %d, want the four workflow moments after the earlier one: %#v", len(sends), sends)
	}
	for _, send := range sends[1:] {
		if !send.HiddenThread || send.Target.ThreadID != workflowThreadID {
			t.Fatalf("send = %#v, want a hidden-thread send about the workflow thread", send)
		}
		if send.Title != "Phase 2: implement" {
			t.Fatalf("title = %q, want the workflow thread's own", send.Title)
		}
	}

	// The opt-in narrows the kinds; it does not replace them. A screen that
	// turned turn completions off hears nothing about a hidden thread's
	// completion even with the opt-in on.
	if _, err := app.settings.BackendScreen().Update(map[string]any{"notifyTurnComplete": false}); err != nil {
		t.Fatalf("update turn-complete setting: %v", err)
	}
	if sends := settled(t, app, recorder, moments[0]); len(sends) != 5 {
		t.Fatalf("sends = %#v, want the kind toggle still to apply", sends)
	}
}

// TestAHiddenThreadsRetractionIsNeverGated: the rest notification a screen
// opted into hearing is withdrawn when the thread resumes, even if the
// opt-in was flipped off in between.
func TestAHiddenThreadsRetractionIsNeverGated(t *testing.T) {
	const workflowThreadID = "thread-workflow-phase"
	app, recorder := newNotificationMappingApp(t)
	phase := testThread(workflowThreadID)
	phase.Mode = threadmode.ModeWorkflow
	if err := app.store.CreateThread(phase); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	hiddenThreads(t, app, true)
	sends := settled(t, app, recorder, func() {
		app.emit(eventchan.ProviderTurnCompleted, triage.TurnCompletedEvent{
			ThreadID: workflowThreadID, TurnID: "turn-1", CountsAsActivity: true,
		})
	})
	if len(sends) != 1 {
		t.Fatalf("sends = %#v, want the opted-in completion", sends)
	}
	hiddenThreads(t, app, false)
	sends = settled(t, app, recorder, func() {
		app.emit(eventchan.ProviderTurnStarted, triage.TurnStartedEvent{ThreadID: workflowThreadID, TurnID: "turn-2"})
	})
	if len(sends) != 2 || !sends[1].Retract || sends[1].ID != sends[0].ID {
		t.Fatalf("sends = %#v, want the retraction to survive the opt-in being turned off", sends)
	}
}

// TestTheHotChannelsAreNotMapped keeps the tap honest about its cost: the
// channels that carry a turn's streaming deltas fall through the switch and
// queue nothing.
func TestTheHotChannelsAreNotMapped(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	sends := settled(t, app, recorder, func() {
		for range 100 {
			app.emit(eventchan.ProviderItemEvent, map[string]any{"threadId": mappingThreadID})
			app.emit(eventchan.ProviderSubagentProgress, map[string]any{"threadId": mappingThreadID})
		}
	})
	if len(sends) != 0 {
		t.Fatalf("a hot channel notified: %#v", sends)
	}
}

// TestAMismatchedPayloadIsIgnored: the tap type-asserts, and a channel whose
// payload is not the shape it names must not panic the emitting goroutine.
func TestAMismatchedPayloadIsIgnored(t *testing.T) {
	app, recorder := newNotificationMappingApp(t)
	sends := settled(t, app, recorder, func() {
		app.emit(eventchan.ProviderTurnCompleted, map[string]any{"threadId": mappingThreadID})
		app.emit(eventchan.ProviderApproval, "not an approval")
		app.emit(eventchan.ProviderStatus, nil)
	})
	if len(sends) != 0 {
		t.Fatalf("a mismatched payload notified: %#v", sends)
	}
}
