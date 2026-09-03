package app

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/transport"
)

// The ATTENDED-SCREEN half of the notification gate (app_notifications.go
// screenIsAlreadyLooking), against the transport's per-connection presence.
//
// Nothing here asserts anything about DELIVERY, and that is deliberate: the
// gate decides whether an OS notification is raised and nothing else. The
// tripwire for the other half — that a stated presence never narrows what a
// connection is sent — lives beside the frame, in
// internal/transport/conn_presence_test.go.

// attendedApp is the mapping fixture plus a bus, so a test can state what the
// backend machine's own screen is doing. The subscriber is loopback because
// that is what "the local screen" means.
func attendedApp(t *testing.T) (*App, *recordingNotificationSender, *transport.Subscriber) {
	t.Helper()
	app, recorder := newNotificationMappingApp(t)
	bus := transport.NewEventBus(8)
	t.Cleanup(bus.Close)
	app.SetEventBus(bus)
	subscriber := bus.Subscribe()
	subscriber.SetOriginLoopback(true)
	return app, recorder, subscriber
}

// quietWhen writes the two attended-screen preferences onto the backend
// machine's own screen, which is the screen the gate reads.
func quietWhen(t *testing.T, app *App, focused, threadVisible bool) {
	t.Helper()
	if _, err := app.settings.BackendScreen().Update(map[string]any{
		"notifyMuteWhenFocused":       focused,
		"notifyMuteWhenThreadVisible": threadVisible,
	}); err != nil {
		t.Fatalf("update quiet-when settings: %v", err)
	}
}

func threadSend() notify.Send {
	return notify.Send{
		ID: "thread:" + mappingThreadID, Kind: notify.KindTurnComplete, Title: "t",
		Target: notify.Target{Kind: "thread", ThreadID: mappingThreadID},
	}
}

func wantScreenAttended(t *testing.T, err error, context string) {
	t.Helper()
	var notificationErr *NotificationError
	if !errors.As(err, &notificationErr) || notificationErr.Code != NotificationScreenAttended {
		t.Fatalf("%s: notifyOS = %v, want a screen-attended refusal", context, err)
	}
}

// The headline rule, and the one that defaults ON: a person looking at the
// app on this machine is not interrupted by it.
func TestAFocusedLocalScreenMutesTheNotification(t *testing.T) {
	app, _, subscriber := attendedApp(t)
	subscriber.SetPresence(true, nil)

	wantScreenAttended(t, app.notifyOS(threadSend()), "a focused screen")

	// And it is the PREFERENCE that decides, not the presence.
	quietWhen(t, app, false, false)
	if err := app.notifyOS(threadSend()); err != nil {
		t.Fatalf("with both quiet preferences off, notifyOS = %v, want the notification raised", err)
	}
}

// The second rule is independent of the first, and applies only to a send
// whose target NAMES a thread: a workflow item or an update notice has no
// thread for a pane to be showing.
func TestTheThreadVisibleRuleAppliesOnlyToAThreadTarget(t *testing.T) {
	app, _, subscriber := attendedApp(t)
	quietWhen(t, app, false, true)
	// Unfocused — another app is in front — with the thread's pane on screen.
	subscriber.SetPresence(false, []string{mappingThreadID})

	wantScreenAttended(t, app.notifyOS(threadSend()), "the thread's own pane visible")

	targetless := notify.Send{
		ID: "workflow:item", Kind: notify.KindWorkflowAttention, Title: "t",
		Target: notify.Target{Kind: "none"},
	}
	if err := app.notifyOS(targetless); err != nil {
		t.Fatalf("a send naming no thread was muted by the thread rule: %v", err)
	}

	other := threadSend()
	other.ID = "thread:other"
	other.Target.ThreadID = "thread-not-on-screen"
	if err := app.notifyOS(other); err != nil {
		t.Fatalf("a send about a thread nobody is showing was muted: %v", err)
	}
}

// A REMOTE screen is somebody else's. A phone the owner is staring at must
// never silence the machine sitting in front of them.
func TestARemoteScreenNeverMutesTheDesktop(t *testing.T) {
	app, _, subscriber := attendedApp(t)
	subscriber.SetOriginLoopback(false)
	subscriber.SetPresence(true, []string{mappingThreadID})
	quietWhen(t, app, true, true)

	if err := app.notifyOS(threadSend()); err != nil {
		t.Fatalf("a remote client's focus muted this machine: %v", err)
	}
}

// A RETRACTION IS NEVER GATED, by this half either. Somebody walking back to
// their desk between a send and its withdrawal must not be left with the
// notification forever.
func TestAnAttendedScreenNeverGatesARetraction(t *testing.T) {
	app, recorder, subscriber := attendedApp(t)
	subscriber.SetPresence(true, []string{mappingThreadID})
	quietWhen(t, app, true, true)

	retraction := notify.Send{ID: "thread:" + mappingThreadID, Kind: notify.KindTurnComplete, Retract: true}
	if err := app.notifyOS(retraction); err != nil {
		t.Fatalf("notifyOS(retract) = %v, want the withdrawal to go through", err)
	}
	sends := recorder.snapshot()
	if len(sends) != 1 || !sends[0].Retract {
		t.Fatalf("presenter saw %#v, want exactly the retraction", sends)
	}
}

// No transport at all — every fixture that never wired a bus, and this
// process before one exists — is NOT attended. "Nobody has told us anything"
// has to raise the notification, which is the behavior before the gate.
func TestWithNoTransportNoScreenIsAttended(t *testing.T) {
	app, _ := newNotificationMappingApp(t)
	quietWhen(t, app, true, true)

	if err := app.notifyOS(threadSend()); err != nil {
		t.Fatalf("notifyOS with no event bus = %v, want the notification raised", err)
	}
}

// A connection that never stated a presence is not a screen either, so the
// frame stays additive: every client predating it behaves as it always did.
func TestAConnectionThatStatedNothingIsNotAScreen(t *testing.T) {
	app, _, _ := attendedApp(t)
	quietWhen(t, app, true, true)

	if err := app.notifyOS(threadSend()); err != nil {
		t.Fatalf("notifyOS with a silent connection = %v, want the notification raised", err)
	}
}

// Neither gate outcome is a fault. Logging one would put a line in the log
// every time somebody watched a turn they were watching finish.
func TestNeitherGateOutcomeIsLoggedAsAFailure(t *testing.T) {
	app, _ := newNotificationMappingApp(t)
	app.logNotificationFailure(&NotificationError{Code: NotificationScreenAttended})
	app.logNotificationFailure(&NotificationError{Code: NotificationSuppressed})
	if len(app.notifications.loggedCodes) != 0 {
		t.Fatalf("logged codes = %v, want neither gate outcome recorded", app.notifications.loggedCodes)
	}
	app.logNotificationFailure(&NotificationError{Code: NotificationDeliveryFailed})
	if _, logged := app.notifications.loggedCodes[NotificationDeliveryFailed]; !logged {
		t.Fatal("a real delivery failure was not logged")
	}
}

// THE PUSH FAN-OUT IS NOT SUBJECT TO THESE TWO GATES. A phone in a pocket is
// a different screen from the one the presence describes, and the desktop
// being looked at says nothing about whether that phone should buzz. The
// per-kind toggles still apply there, per phone, which the push tests cover.
func TestTheAttendedScreenGatesDoNotReachThePhones(t *testing.T) {
	app, sender := pushApp(t)
	bus := transport.NewEventBus(8)
	t.Cleanup(bus.Close)
	app.SetEventBus(bus)
	subscriber := bus.Subscribe()
	subscriber.SetOriginLoopback(true)
	subscriber.SetPresence(true, []string{mappingThreadID})
	if _, err := app.settings.BackendScreen().Update(map[string]any{
		"notifyMuteWhenFocused":       true,
		"notifyMuteWhenThreadVisible": true,
	}); err != nil {
		t.Fatalf("update quiet-when settings: %v", err)
	}
	pairPhone(t, app, "thumb-phone", "token-phone")

	if messages := firedPush(t, app, turnCompleteSend()); len(messages) != 1 {
		t.Fatalf("messages = %d, want the phone woken while the desktop is being watched", len(messages))
	}
	_ = sender
}

// The OTHER half of the gate's totality, pinned here beside the bypass
// because the two tripwires answer one question together: can a send reach a
// presenter without a preference having judged it?
//
// notificationKindEnabledIn is TOTAL with a fail-closed default, so a kind
// added without a settings key is not a compile error — it is a moment that
// silently stops being raised. The list is read out of internal/notify's own
// source, because a copy of it here is the thing that goes stale.
func TestEveryDeclaredKindHasAToggleThatDefaultsOn(t *testing.T) {
	const mappingPath = "internal/notify/mapping.go"
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, mappingPath, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", mappingPath, err)
	}
	declared := map[notify.Kind]string{}
	for _, decl := range parsed.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}
		for _, spec := range genDecl.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			typeName, ok := value.Type.(*ast.Ident)
			if !ok || typeName.Name != "Kind" || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}
			literal, ok := value.Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				continue
			}
			unquoted, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("%s: unquote %s: %v", mappingPath, value.Names[0].Name, err)
			}
			declared[notify.Kind(unquoted)] = value.Names[0].Name
		}
	}
	if len(declared) < 6 {
		t.Fatalf("found %d declared kinds in %s, want every one of them", len(declared), mappingPath)
	}
	for kind, name := range declared {
		if !notify.KnownKind(kind) {
			t.Errorf("%s = %q is not a kind ValidateSend accepts", name, kind)
		}
		if !notificationKindEnabledIn(settings.DefaultSettings, kind) {
			t.Errorf(
				"%s (%q) is off under DefaultSettings: it either has no toggle at all — in which "+
					"case the fail-closed default is silently swallowing it — or its key was left "+
					"out of DefaultSettings. Notifications were unconditional before these keys "+
					"existed, so every kind defaults ON.",
				name, kind,
			)
		}
	}
}

// notifyOSUngated skips both gates, so its caller list is the whole of what
// makes that safe. One caller, and it is the harness RPC — a send that
// exercises the pipe rather than reporting a moment.
func TestOnlyTheHarnessBypassesTheNotificationGate(t *testing.T) {
	const packageDir = "internal/app"
	allowed := map[string]string{
		"app_notifications.go": "declares it, and calls it as notifyOS's own presentation half",
		"app_harness.go":       "the harness RPC, whose send must not depend on preferences or a Playwright page's focus",
	}
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		t.Fatalf("read %s: %v", packageDir, err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(packageDir, name)
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if !ok || ident.Name != "notifyOSUngated" {
				return true
			}
			seen[name] = true
			if _, ok := allowed[name]; !ok {
				t.Errorf(
					"%s:%d names notifyOSUngated, which skips the per-kind and "+
						"attended-screen gates. Send through notifyOS instead, or say here "+
						"why this caller describes no real state.",
					path, fileSet.Position(ident.Pos()).Line,
				)
			}
			return true
		})
	}
	for name, why := range allowed {
		if !seen[name] {
			t.Errorf("%s no longer names notifyOSUngated; drop the entry (%s)", name, why)
		}
	}
}
