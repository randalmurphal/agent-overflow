package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/transport"
)

func TestNotifyOSUnavailableIsTypedAndVisible(t *testing.T) {
	err := (&App{}).notifyOS("Done", "The task completed", notify.Target{Kind: "none"})
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
	err := a.notifyOS(strings.Repeat("x", notify.MaxTitleBytes+1), "body", notify.Target{Kind: "none"})
	if err == nil || !strings.Contains(err.Error(), "title exceeds") {
		t.Fatalf("oversized title error = %v", err)
	}
	err = a.notifyOS("title", strings.Repeat("x", notify.MaxBodyBytes+1), notify.Target{Kind: "none"})
	if err == nil || !strings.Contains(err.Error(), "body exceeds") {
		t.Fatalf("oversized body error = %v", err)
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

	if err := a.notifyOS("Ready", "Open the finished thread", target); err != nil {
		t.Fatalf("notifyOS: %v", err)
	}
	events := bus.Replay(map[string]uint64{notify.SendChannel: 0})
	if len(events) != 1 {
		t.Fatalf("replayed events = %d, want 1", len(events))
	}
	var got notify.Send
	if err := json.Unmarshal(events[0].Data, &got); err != nil {
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
	h := &Harness{app: a}

	err := h.HarnessNotify("Ready", "Body", notify.Target{Kind: "none"})
	var notificationErr *NotificationError
	if !errors.As(err, &notificationErr) || notificationErr.Code != NotificationUnavailable {
		t.Fatalf("HarnessNotify error = %v, want unavailable notification error", err)
	}
	if eventName != notify.ActivatedChannel {
		t.Fatalf("activation event = %q, want %q", eventName, notify.ActivatedChannel)
	}
}
