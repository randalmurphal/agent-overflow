package wsllauncher

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/selfupdate"
)

func newTestTrimClient(t *testing.T, handleTrim func(string)) (*NotificationClient, *bridgeLog) {
	t.Helper()
	logs := &bridgeLog{}
	client, err := NewNotificationClient(NotificationClientConfig{
		WSURL:             "ws://127.0.0.1/ws",
		Token:             "test-token",
		Present:           func(notify.Send) error { return nil },
		Logf:              logs.logf,
		HandleWebviewTrim: handleTrim,
	})
	if err != nil {
		t.Fatalf("NewNotificationClient: %v", err)
	}
	return client, logs
}

// The channels slice IS the subscribe-frame payload (fixed at construction,
// resent on every reconnect), so asserting it covers the wire subscription
// the same way the install-channel tests do over a live socket.
func TestNotificationClientSubscribeChannelsFollowWebviewTrimHandler(t *testing.T) {
	withoutTrim, _ := newTestBridgeClient(t, "ws://127.0.0.1/ws", func(selfupdate.InstallDirective) {})
	if slices.Contains(withoutTrim.channels, string(eventchan.WebviewTrim)) {
		t.Fatalf("channels = %v; webview:trim subscribed with no handler", withoutTrim.channels)
	}

	withTrim, _ := newTestTrimClient(t, func(string) {})
	want := []string{notify.SendChannel, string(eventchan.WebviewTrim)}
	if !slices.Equal(withTrim.channels, want) {
		t.Fatalf("channels = %v, want %v", withTrim.channels, want)
	}
}

func TestNotificationClientDispatchesWebviewTrimDirective(t *testing.T) {
	dispatched := make(chan string, 1)
	client, _ := newTestTrimClient(t, func(reason string) { dispatched <- reason })

	if err := client.handleEvent(notificationEvent{
		Channel: string(eventchan.WebviewTrim),
		Seq:     7,
		Data:    json.RawMessage(`{"reason":"idle"}`),
	}); err != nil {
		t.Fatalf("handleEvent = %v, want nil", err)
	}
	select {
	case got := <-dispatched:
		if got != "idle" {
			t.Fatalf("dispatched reason = %q, want %q", got, "idle")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the trim handler")
	}
}

func TestNotificationClientDropsMalformedWebviewTrimDirective(t *testing.T) {
	dispatched := make(chan string, 1)
	client, logs := newTestTrimClient(t, func(reason string) { dispatched <- reason })

	if err := client.handleEvent(notificationEvent{
		Channel: string(eventchan.WebviewTrim),
		Seq:     1,
		Data:    json.RawMessage(`{"reason":`),
	}); err != nil {
		t.Fatalf("handleEvent = %v, want nil (a bad directive must not kill the connection)", err)
	}
	if !logs.contains("ignore malformed directive") {
		t.Fatalf("logs = %v, want a line about the malformed directive", logs.lines())
	}
	select {
	case got := <-dispatched:
		t.Fatalf("handler ran for a malformed directive: %q", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// Like the install channel, webview:trim is ephemeral: its sequence numbers
// must never fold into notification:send's replay cursor.
func TestNotificationClientWebviewTrimEventDoesNotDisturbSendCursor(t *testing.T) {
	client, _ := newTestTrimClient(t, func(string) {})
	client.lastSeq = 4

	if err := client.handleEvent(notificationEvent{
		Channel: string(eventchan.WebviewTrim),
		Seq:     99,
		Data:    json.RawMessage(`{"reason":"idle"}`),
	}); err != nil {
		t.Fatalf("handleEvent = %v, want nil", err)
	}
	if client.lastSeq != 4 {
		t.Fatalf("last notification sequence = %d, want 4 untouched", client.lastSeq)
	}
}
