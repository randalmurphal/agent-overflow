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

func newTestKeepAwakeClient(t *testing.T, handle func(string)) (*NotificationClient, *bridgeLog) {
	t.Helper()
	logs := &bridgeLog{}
	client, err := NewNotificationClient(NotificationClientConfig{
		WSURL:           "ws://127.0.0.1/ws",
		Token:           "test-token",
		Present:         func(notify.Send) error { return nil },
		Logf:            logs.logf,
		HandleKeepAwake: handle,
	})
	if err != nil {
		t.Fatalf("NewNotificationClient: %v", err)
	}
	return client, logs
}

func TestNotificationClientSubscribeChannelsFollowKeepAwakeHandler(t *testing.T) {
	without, _ := newTestBridgeClient(t, "ws://127.0.0.1/ws", func(selfupdate.InstallDirective) {})
	if slices.Contains(without.channels, string(eventchan.PowerKeepAwake)) {
		t.Fatalf("channels = %v; power:keepawake subscribed with no handler", without.channels)
	}
	if len(without.levelChannels) != 0 {
		t.Fatalf("levelChannels = %v, want none with no keep-awake handler", without.levelChannels)
	}

	with, _ := newTestKeepAwakeClient(t, func(string) {})
	want := []string{notify.SendChannel, string(eventchan.PowerKeepAwake)}
	if !slices.Equal(with.channels, want) {
		t.Fatalf("channels = %v, want %v", with.channels, want)
	}
	// The level list is what puts a ZERO cursor on the replay frame, which
	// is the entire reconnect-convergence mechanism: the server's
	// latest-only ring answers a zero cursor with its newest frame however
	// long ago it was emitted.
	if !slices.Equal(with.levelChannels, []string{string(eventchan.PowerKeepAwake)}) {
		t.Fatalf("levelChannels = %v, want [power:keepawake]", with.levelChannels)
	}
}

func TestNotificationClientDispatchesKeepAwakeDirective(t *testing.T) {
	for _, mode := range []string{"off", "system", "display"} {
		dispatched := make(chan string, 1)
		client, _ := newTestKeepAwakeClient(t, func(m string) { dispatched <- m })

		if err := client.handleEvent(notificationEvent{
			Channel: string(eventchan.PowerKeepAwake),
			Seq:     3,
			Data:    json.RawMessage(`{"mode":"` + mode + `"}`),
		}); err != nil {
			t.Fatalf("handleEvent = %v, want nil", err)
		}
		select {
		case got := <-dispatched:
			if got != mode {
				t.Fatalf("dispatched mode = %q, want %q", got, mode)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for the keep-awake handler on mode %q", mode)
		}
	}
}

// Keep-awake is a LEVEL: frames must apply in arrival order, and when
// they outpace the handler only the newest matters. A goroutine per
// frame (the webview:trim dispatch shape) would let a stale mode apply
// last after a fast on/off toggle, leaving the machine's power state
// contradicting the setting.
func TestNotificationClientKeepAwakeDirectivesApplyInOrderLatestWins(t *testing.T) {
	applied := make(chan string, 8)
	gate := make(chan struct{})
	started := make(chan struct{})
	first := true
	// The handler only ever runs on the single drain goroutine, so
	// `first` needs no lock — that serialization is the property under
	// test.
	client, _ := newTestKeepAwakeClient(t, func(m string) {
		if first {
			first = false
			close(started)
			<-gate
		}
		applied <- m
	})

	deliver := func(mode string) {
		t.Helper()
		if err := client.handleEvent(notificationEvent{
			Channel: string(eventchan.PowerKeepAwake),
			Seq:     1,
			Data:    json.RawMessage(`{"mode":"` + mode + `"}`),
		}); err != nil {
			t.Fatalf("handleEvent(%q) = %v, want nil", mode, err)
		}
	}
	receive := func() string {
		t.Helper()
		select {
		case got := <-applied:
			return got
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for the keep-awake handler")
			return ""
		}
	}

	// Park the drain inside the first handler before sending the burst. Without
	// this handshake, the drain may consume the first "off" before the final
	// "off" arrives; the output values are identical and a test cannot tell an
	// intermediate application from convergence.
	deliver("display")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first keep-awake handler to start")
	}
	deliver("off")
	deliver("display")
	deliver("off")
	close(gate)
	if got := receive(); got != "display" {
		t.Fatalf("first applied mode = %q, want %q", got, "display")
	}
	if got := receive(); got != "off" {
		t.Fatalf("converged mode = %q, want newest frame %q", got, "off")
	}
	select {
	case extra := <-applied:
		t.Fatalf("extra handler run %q after convergence; stale modes must not re-apply", extra)
	case <-time.After(50 * time.Millisecond):
	}

	// The drain exits when the mailbox empties; a later frame must
	// restart it rather than vanish.
	deliver("system")
	if got := receive(); got != "system" {
		t.Fatalf("post-drain applied mode = %q, want %q", got, "system")
	}
}

// A directive that cannot be read must not be defaulted: guessing "on"
// pins the machine awake on a garbled frame, and guessing "off" drops an
// inhibit the user asked for. Neither may be connection-fatal either.
func TestNotificationClientDropsUnreadableKeepAwakeDirective(t *testing.T) {
	cases := []struct {
		name    string
		data    json.RawMessage
		wantLog string
	}{
		{"malformed json", json.RawMessage(`{"mode":`), "ignore malformed directive"},
		{"no mode", json.RawMessage(`{}`), "ignore directive with no mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dispatched := make(chan string, 1)
			client, logs := newTestKeepAwakeClient(t, func(m string) { dispatched <- m })

			if err := client.handleEvent(notificationEvent{
				Channel: string(eventchan.PowerKeepAwake),
				Seq:     1,
				Data:    tc.data,
			}); err != nil {
				t.Fatalf("handleEvent = %v, want nil (a bad directive must not kill the bridge)", err)
			}
			if !logs.contains(tc.wantLog) {
				t.Fatalf("logs = %v, want a line containing %q", logs.lines(), tc.wantLog)
			}
			select {
			case got := <-dispatched:
				t.Fatalf("handler ran for an unreadable directive: %q", got)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

// A gap marker carries `null` and no mode. It cannot happen with a zero
// cursor on a latest-only ring, but acting on one would mean guessing at
// the machine's power state.
func TestNotificationClientIgnoresKeepAwakeGapMarker(t *testing.T) {
	dispatched := make(chan string, 1)
	client, logs := newTestKeepAwakeClient(t, func(m string) { dispatched <- m })

	if err := client.handleEvent(notificationEvent{
		Channel: string(eventchan.PowerKeepAwake),
		Seq:     12,
		Gap:     true,
		Data:    json.RawMessage(`null`),
	}); err != nil {
		t.Fatalf("handleEvent = %v, want nil", err)
	}
	if !logs.contains("ignore replay gap marker") {
		t.Fatalf("logs = %v, want a line about the gap marker", logs.lines())
	}
	select {
	case got := <-dispatched:
		t.Fatalf("handler ran for a gap marker: %q", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// The directive channel must never fold into notification:send's replay
// cursor — that cursor is history, this one is a level.
func TestNotificationClientKeepAwakeEventDoesNotDisturbSendCursor(t *testing.T) {
	client, _ := newTestKeepAwakeClient(t, func(string) {})
	client.lastSeq = 6

	if err := client.handleEvent(notificationEvent{
		Channel: string(eventchan.PowerKeepAwake),
		Seq:     91,
		Data:    json.RawMessage(`{"mode":"display"}`),
	}); err != nil {
		t.Fatalf("handleEvent = %v, want nil", err)
	}
	if client.lastSeq != 6 {
		t.Fatalf("last notification sequence = %d, want 6 untouched", client.lastSeq)
	}
}
