package wsllauncher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/selfupdate"
	"agent-overflow/internal/webview2host"

	"github.com/coder/websocket"
)

func newTestBrowserHostClient(t *testing.T, wsURL string, handle func(webview2host.Directive)) (*NotificationClient, *bridgeLog) {
	t.Helper()
	logs := &bridgeLog{}
	client, err := NewNotificationClient(NotificationClientConfig{
		WSURL:             wsURL,
		Token:             "test-token",
		Present:           func(notify.Send) error { return nil },
		Logf:              logs.logf,
		HandleBrowserHost: handle,
		MinBackoff:        time.Millisecond,
		MaxBackoff:        5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewNotificationClient: %v", err)
	}
	return client, logs
}

// The channels slice IS the subscribe-frame payload, fixed at construction
// and resent on every reconnect, so asserting it covers the wire
// subscription without a live socket.
func TestNotificationClientSubscribeChannelsFollowBrowserHostHandler(t *testing.T) {
	withoutHost, _ := newTestBridgeClient(t, "ws://127.0.0.1/ws", func(selfupdate.InstallDirective) {})
	if slices.Contains(withoutHost.channels, string(eventchan.BrowserHost)) {
		t.Fatalf("channels = %v; browser:host subscribed with no handler", withoutHost.channels)
	}

	withHost, _ := newTestBrowserHostClient(t, "ws://127.0.0.1/ws", func(webview2host.Directive) {})
	want := []string{notify.SendChannel, string(eventchan.BrowserHost)}
	if !slices.Equal(withHost.channels, want) {
		t.Fatalf("channels = %v, want %v", withHost.channels, want)
	}
}

func TestNotificationClientDispatchesBrowserHostDirective(t *testing.T) {
	dispatched := make(chan webview2host.Directive, 1)
	client, _ := newTestBrowserHostClient(t, "ws://127.0.0.1/ws", func(got webview2host.Directive) {
		dispatched <- got
	})

	const payload = `{"op":"bounds","pageId":"page-1","profileId":"ws-main","x":12,"y":34,"w":800,"h":600}`
	if err := client.handleEvent(notificationEvent{
		Channel: string(eventchan.BrowserHost),
		Seq:     3,
		Data:    json.RawMessage(payload),
	}); err != nil {
		t.Fatalf("handleEvent = %v, want nil", err)
	}
	select {
	case got := <-dispatched:
		want := webview2host.Directive{
			Op:        webview2host.OpBounds,
			PageID:    "page-1",
			ProfileID: "ws-main",
			X:         12,
			Y:         34,
			W:         800,
			H:         600,
		}
		if got != want {
			t.Fatalf("dispatched directive = %#v, want %#v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the browser host handler")
	}
}

// The trust boundary: a directive drives real browser windows and names a
// per-workspace profile that becomes a directory component, so anything
// that fails Validate must never reach the handler, and none of it may
// take the connection down.
func TestNotificationClientDropsUnusableBrowserHostDirective(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantLog string
	}{
		{"junk json", `{"op":`, "ignore malformed directive"},
		{"unknown op", `{"op":"teleport","pageId":"page-1"}`, "ignore invalid directive"},
		{"empty op", `{"pageId":"page-1"}`, "ignore invalid directive"},
		{"create without a page id", `{"op":"create","profileId":"ws-main"}`, "ignore invalid directive"},
		{"create without a profile id", `{"op":"create","pageId":"page-1"}`, "ignore invalid directive"},
		{"page id with a path separator", `{"op":"show","pageId":"../page"}`, "ignore invalid directive"},
		{"profile id with a path separator", `{"op":"create","pageId":"page-1","profileId":"../escape"}`, "ignore invalid directive"},
		{"absurd bounds", `{"op":"bounds","pageId":"page-1","x":0,"y":0,"w":1e9,"h":600}`, "ignore invalid directive"},
		{"negative size", `{"op":"bounds","pageId":"page-1","x":0,"y":0,"w":-10,"h":600}`, "ignore invalid directive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dispatched := make(chan webview2host.Directive, 1)
			client, logs := newTestBrowserHostClient(t, "ws://127.0.0.1/ws", func(got webview2host.Directive) {
				dispatched <- got
			})

			if err := client.handleEvent(notificationEvent{
				Channel: string(eventchan.BrowserHost),
				Seq:     1,
				Data:    json.RawMessage(tc.payload),
			}); err != nil {
				t.Fatalf("handleEvent = %v, want nil (a bad directive must not kill the connection)", err)
			}
			if !logs.contains(tc.wantLog) {
				t.Fatalf("logs = %v, want a line containing %q", logs.lines(), tc.wantLog)
			}
			select {
			case got := <-dispatched:
				t.Fatalf("handler ran for an unusable directive: %#v", got)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

// browser:host is ephemeral like the install and trim channels: its
// sequence numbers are independent of notification:send's, so folding them
// into the replay cursor would fabricate gaps.
func TestNotificationClientBrowserHostEventDoesNotDisturbSendCursor(t *testing.T) {
	client, _ := newTestBrowserHostClient(t, "ws://127.0.0.1/ws", func(webview2host.Directive) {})
	client.lastSeq = 4

	if err := client.handleEvent(notificationEvent{
		Channel: string(eventchan.BrowserHost),
		Seq:     99,
		Data:    json.RawMessage(`{"op":"hide","pageId":"page-1"}`),
	}); err != nil {
		t.Fatalf("handleEvent = %v, want nil", err)
	}
	if client.lastSeq != 4 {
		t.Fatalf("last notification sequence = %d, want 4 untouched", client.lastSeq)
	}
}

func TestNotificationClientReportsBrowserHost(t *testing.T) {
	rpcs := make(chan notificationClientFrame, 2)
	wsURL := startBridgeStub(t, func(ctx context.Context, conn *websocket.Conn, _ int) error {
		if err := expectBrowserHostSubscribeAndReplay(ctx, conn); err != nil {
			return err
		}
		for call := 0; ; call++ {
			frame, err := readClientFrame(ctx, conn)
			if err != nil {
				return nil
			}
			if frame.Type != "rpc" {
				return fmt.Errorf("frame type = %q, want rpc", frame.Type)
			}
			rpcs <- frame
			response := notificationServerFrame{Type: "rpc", ID: frame.ID}
			if call == 1 {
				response.Error = &notificationFrameError{Code: "method_error", Message: "no such page"}
			}
			if err := writeServerFrame(ctx, conn, response); err != nil {
				return nil
			}
		}
	})

	client, _ := newTestBrowserHostClient(t, wsURL, func(webview2host.Directive) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer callCancel()
	if err := client.ReportBrowserHost(callCtx, "page-1", webview2host.ReportCreated, "7A1F0C2D"); err != nil {
		t.Fatalf("ReportBrowserHost: %v", err)
	}
	assertBrowserHostRPC(t, rpcs, "page-1", string(webview2host.ReportCreated), "7A1F0C2D")

	err := client.ReportBrowserHost(callCtx, "page-1", webview2host.ReportClosed, "")
	var refused *RPCRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v (%T), want a *RPCRefusedError", err, err)
	}
	if refused.Method != webview2host.RPCReport || refused.Code != "method_error" {
		t.Fatalf("refusal = %#v, want method %q / code method_error", refused, webview2host.RPCReport)
	}
	assertBrowserHostRPC(t, rpcs, "page-1", string(webview2host.ReportClosed), "")
}

// A typo in launcher code would otherwise reach the backend as a report it
// can only drop, so both arguments are checked before the wire.
func TestReportBrowserHostRejectsUnusableArgumentsBeforeTheWire(t *testing.T) {
	client, _ := newTestBrowserHostClient(t, "ws://127.0.0.1:1/ws", func(webview2host.Directive) {})
	client.rpcTimeout = 50 * time.Millisecond
	// No Run loop: reaching the wire at all would mean blocking here, so a
	// prompt error is itself evidence the check ran first.

	ctx := context.Background()
	if err := client.ReportBrowserHost(ctx, "../escape", webview2host.ReportCreated, ""); err == nil {
		t.Fatal("ReportBrowserHost accepted a page id with a path separator")
	}
	err := client.ReportBrowserHost(ctx, "page-1", webview2host.ReportKind("exploded"), "")
	if err == nil {
		t.Fatal("ReportBrowserHost accepted an unknown report kind")
	}
	if !strings.Contains(err.Error(), "exploded") {
		t.Fatalf("error = %q, want it to name the rejected kind", err)
	}
}

// Detail is model-free launcher text, but it is still unbounded input from
// a COM error string, so the wire carries a truncated copy rather than
// whatever the OS produced.
func TestReportBrowserHostTruncatesDetail(t *testing.T) {
	rpcs := make(chan notificationClientFrame, 1)
	wsURL := startBridgeStub(t, func(ctx context.Context, conn *websocket.Conn, _ int) error {
		if err := expectBrowserHostSubscribeAndReplay(ctx, conn); err != nil {
			return err
		}
		frame, err := readClientFrame(ctx, conn)
		if err != nil {
			return nil
		}
		rpcs <- frame
		if err := writeServerFrame(ctx, conn, notificationServerFrame{Type: "rpc", ID: frame.ID}); err != nil {
			return nil
		}
		<-ctx.Done()
		return nil
	})

	client, _ := newTestBrowserHostClient(t, wsURL, func(webview2host.Directive) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer callCancel()
	huge := strings.Repeat("E", webview2host.MaxReportDetailBytes*2)
	if err := client.ReportBrowserHost(callCtx, "page-1", webview2host.ReportProcessFailed, huge); err != nil {
		t.Fatalf("ReportBrowserHost: %v", err)
	}
	select {
	case frame := <-rpcs:
		var detail string
		if err := json.Unmarshal(frame.Params[2], &detail); err != nil {
			t.Fatalf("decode detail: %v", err)
		}
		if len(detail) > webview2host.MaxReportDetailBytes {
			t.Fatalf("detail is %d bytes, want it capped at %d", len(detail), webview2host.MaxReportDetailBytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the browser host RPC")
	}
}

// expectBrowserHostSubscribeAndReplay consumes the client's two opening
// frames and asserts browser:host is subscribed but never replayed.
func expectBrowserHostSubscribeAndReplay(ctx context.Context, conn *websocket.Conn) error {
	subscribe, err := readClientFrame(ctx, conn)
	if err != nil {
		return err
	}
	if subscribe.Type != "subscribe" {
		return fmt.Errorf("first frame type = %q, want subscribe", subscribe.Type)
	}
	if !slices.Contains(subscribe.Channels, string(eventchan.BrowserHost)) {
		return fmt.Errorf("subscribe channels = %v, want %s included", subscribe.Channels, eventchan.BrowserHost)
	}
	replay, err := readClientFrame(ctx, conn)
	if err != nil {
		return err
	}
	if replay.Type != "replay" {
		return fmt.Errorf("second frame type = %q, want replay", replay.Type)
	}
	if _, tracked := replay.LastSeqByChannel[string(eventchan.BrowserHost)]; tracked {
		return fmt.Errorf("replay request tracks %s; the channel is ephemeral and must not be replayed", eventchan.BrowserHost)
	}
	return writeServerFrame(ctx, conn, notificationServerFrame{Type: "replay"})
}

func assertBrowserHostRPC(t *testing.T, rpcs <-chan notificationClientFrame, wantPageID, wantKind, wantDetail string) {
	t.Helper()
	select {
	case frame := <-rpcs:
		if frame.Method != webview2host.RPCReport {
			t.Fatalf("rpc method = %q, want %q", frame.Method, webview2host.RPCReport)
		}
		if len(frame.Params) != 3 {
			t.Fatalf("rpc params = %d, want 3", len(frame.Params))
		}
		got := make([]string, 0, 3)
		for _, raw := range frame.Params {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				t.Fatalf("decode rpc param %s: %v", raw, err)
			}
			got = append(got, value)
		}
		want := []string{wantPageID, wantKind, wantDetail}
		if !slices.Equal(got, want) {
			t.Fatalf("rpc params = %v, want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the browser host RPC")
	}
}
