package app

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/transport"
)

// testSend is a valid presentation with the parts a test does not care
// about filled in. Every notifyOS test builds from it, so a new field on the
// wire shape lands in one place here rather than in a dozen literals.
func testSend(target notify.Target) notify.Send {
	return notify.Send{
		ID:     "test-notification",
		Kind:   notify.KindTurnComplete,
		Title:  "Done",
		Body:   "The task completed",
		Target: target,
	}
}

func TestNotifyOSUnavailableIsTypedAndVisible(t *testing.T) {
	err := (&App{}).notifyOS(testSend(notify.Target{Kind: "none"}))
	var notificationErr *NotificationError
	if !errors.As(err, &notificationErr) {
		t.Fatalf("notifyOS error = %v, want *NotificationError", err)
	}
	if notificationErr.Code != NotificationUnavailable {
		t.Fatalf("notification error code = %q, want %q", notificationErr.Code, NotificationUnavailable)
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("notifyOS error = %q, want visible unavailable message", err)
	}
}

func TestNotifyOSRejectsOversizedContentBeforeRouting(t *testing.T) {
	a := &App{osNotifications: unavailableNotificationSender{reason: errors.New("should not be reached")}}
	oversized := testSend(notify.Target{Kind: "none"})
	oversized.Title = strings.Repeat("x", notify.MaxTitleBytes+1)
	if err := a.notifyOS(oversized); err == nil || !strings.Contains(err.Error(), "title exceeds") {
		t.Fatalf("oversized title error = %v", err)
	}
	oversized = testSend(notify.Target{Kind: "none"})
	oversized.Body = strings.Repeat("x", notify.MaxBodyBytes+1)
	if err := a.notifyOS(oversized); err == nil || !strings.Contains(err.Error(), "body exceeds") {
		t.Fatalf("oversized body error = %v", err)
	}
}

// TestNotifyOSRefusesAnUndeclaredKind: the preference gate switches on the
// kind, so a send naming one this build has never heard of is a preference
// nobody can express. It is refused at the pipe rather than defaulted into
// "always show".
func TestNotifyOSRefusesAnUndeclaredKind(t *testing.T) {
	a := &App{osNotifications: unavailableNotificationSender{reason: errors.New("should not be reached")}}
	send := testSend(notify.Target{Kind: "none"})
	send.Kind = "turn-finished-maybe"
	if err := a.notifyOS(send); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("undeclared kind error = %v", err)
	}
}

func TestActivateNotificationTargetEmitsTypedPayload(t *testing.T) {
	var eventName string
	var payload any
	a := &App{testEmitHook: func(name string, data any) {
		eventName = name
		payload = data
	}}
	target := notify.Target{Kind: "thread", ThreadID: "thread-123"}
	if err := a.NotificationActivated(target); err != nil {
		t.Fatalf("NotificationActivated: %v", err)
	}
	if eventName != notify.ActivatedChannel {
		t.Fatalf("event name = %q, want %q", eventName, notify.ActivatedChannel)
	}
	if !reflect.DeepEqual(payload, target) {
		t.Fatalf("payload = %#v, want %#v", payload, target)
	}
}

func TestTransportNotificationSenderPublishesTypedPayload(t *testing.T) {
	bus := transport.NewEventBus(8)
	t.Cleanup(bus.Close)
	a := &App{}
	a.SetEventBus(bus)
	a.osNotifications = newTransportNotificationSender(a)
	target := notify.Target{Kind: "thread", ThreadID: "thread-123"}

	send := testSend(target)
	send.Title, send.Body = "Ready", "Open the finished thread"
	if err := a.notifyOS(send); err != nil {
		t.Fatalf("notifyOS: %v", err)
	}
	events := bus.Replay(map[string]uint64{notify.SendChannel: 0})
	if len(events) != 1 {
		t.Fatalf("replayed events = %d, want 1", len(events))
	}
	// Replayed events carry the pre-encoded wire frame only (the ring
	// drops Data to avoid double retention) — decode through the frame.
	var frame transport.ServerFrame
	if err := json.Unmarshal(events[0].WireBytes, &frame); err != nil {
		t.Fatalf("decode notification wire frame: %v", err)
	}
	var got notify.Send
	if err := json.Unmarshal(frame.Data, &got); err != nil {
		t.Fatalf("decode notification send: %v", err)
	}
	if got.ID == "" || got.Title != "Ready" || got.Body != "Open the finished thread" {
		t.Fatalf("notification payload = %#v", got)
	}
	if !reflect.DeepEqual(got.Target, target) {
		t.Fatalf("target = %#v, want %#v", got.Target, target)
	}
}

func TestHarnessNotifySurfacesDegradedSendAndSynthesizesActivation(t *testing.T) {
	var eventName string
	a := &App{testEmitHook: func(name string, _ any) { eventName = name }}
	a.osNotifications = unavailableNotificationSender{reason: errors.New("headless harness")}
	h := NewHarness(a, HarnessPaths{})

	err := h.HarnessNotify("Ready", "Body", notify.Target{Kind: "none"})
	var notificationErr *NotificationError
	if !errors.As(err, &notificationErr) || notificationErr.Code != NotificationUnavailable {
		t.Fatalf("HarnessNotify error = %v, want unavailable notification error", err)
	}
	if eventName != notify.ActivatedChannel {
		t.Fatalf("activation event = %q, want %q", eventName, notify.ActivatedChannel)
	}
}

// TestIsolatedBootInstallsTheRealNotificationSender: --harness and --soak
// used to take a refusal stub, which made HarnessNotify answer "OS
// notifications are unavailable" before emitting anything — so the e2e
// spec covering the notification pipe asserted the stub's error text and
// the emission path was never executed at all.
func TestIsolatedBootInstallsTheRealNotificationSender(t *testing.T) {
	app := NewApp()
	ConfigureIsolation(app, IsolationConfig{
		CredentialHome: t.TempDir(),
		ProviderBinary: "/nonexistent/ao-mockprovider",
	})
	if _, ok := app.osNotifications.(*transportNotificationSender); !ok {
		t.Fatalf("isolated boot installed %T, want the production transport sender", app.osNotifications)
	}

	// And it actually emits: the bus is wired after the App is built (as
	// bootTransport does), which the sender must tolerate because it
	// resolves the bus per send.
	bus := transport.NewEventBus(8)
	t.Cleanup(bus.Close)
	app.SetEventBus(bus)

	h := NewHarness(app, HarnessPaths{})
	target := notify.Target{Kind: "thread", ThreadID: "thread-abc"}
	if err := h.HarnessNotify("Ready", "Open the finished thread", target); err != nil {
		t.Fatalf("HarnessNotify on an isolated boot: %v", err)
	}

	events := bus.Replay(map[string]uint64{notify.SendChannel: 0})
	if len(events) != 1 {
		t.Fatalf("replayed notification:send events = %d, want 1", len(events))
	}
	var frame transport.ServerFrame
	if err := json.Unmarshal(events[0].WireBytes, &frame); err != nil {
		t.Fatalf("decode wire frame: %v", err)
	}
	var got notify.Send
	if err := json.Unmarshal(frame.Data, &got); err != nil {
		t.Fatalf("decode notification send: %v", err)
	}
	if got.ID == "" || got.Title != "Ready" || !reflect.DeepEqual(got.Target, target) {
		t.Fatalf("notification payload = %#v", got)
	}
}

// TestIsolatedNotificationSendSucceedsWithNoSubscriber pins the
// degradation: a headless harness has no launcher listening, and that
// must be a log line and a success, never an error the caller has to
// special-case.
func TestIsolatedNotificationSendSucceedsWithNoSubscriber(t *testing.T) {
	app := NewApp()
	ConfigureIsolation(app, IsolationConfig{CredentialHome: t.TempDir()})
	bus := transport.NewEventBus(8)
	t.Cleanup(bus.Close)
	app.SetEventBus(bus)

	if got := bus.ChannelSubscriberCount(notify.SendChannel); got != 0 {
		t.Fatalf("fixture has %d explicit subscribers, want none", got)
	}
	if err := app.notifyOS(testSend(notify.Target{Kind: "none"})); err != nil {
		t.Fatalf("send with no subscriber: %v, want success", err)
	}
}
