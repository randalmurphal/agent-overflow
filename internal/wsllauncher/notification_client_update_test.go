package wsllauncher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/selfupdate"

	"github.com/coder/websocket"
)

// validInstallDirective is the shape the backend emits after staging a release.
func validInstallDirective() selfupdate.InstallDirective {
	return selfupdate.InstallDirective{
		Filename: "agent-overflow-wsl-amd64.exe",
		SHA256:   strings.Repeat("ab", 32),
		Version:  "0.0.11",
	}
}

func TestNotificationClientSubscribeChannelsFollowUpdateHandler(t *testing.T) {
	cases := []struct {
		name    string
		handler func(selfupdate.InstallDirective)
		want    []string
	}{
		{"without update handler", nil, []string{notify.SendChannel}},
		{
			"with update handler",
			func(selfupdate.InstallDirective) {},
			[]string{notify.SendChannel, selfupdate.ChannelInstall},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subscribed := make(chan []string, 1)
			wsURL := startBridgeStub(t, func(ctx context.Context, conn *websocket.Conn, _ int) error {
				frame, err := readClientFrame(ctx, conn)
				if err != nil {
					return nil
				}
				if frame.Type != "subscribe" {
					return fmt.Errorf("first frame type = %q, want subscribe", frame.Type)
				}
				subscribed <- frame.Channels
				<-ctx.Done()
				return nil
			})

			client, _ := newTestBridgeClient(t, wsURL, tc.handler)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go client.Run(ctx)

			select {
			case got := <-subscribed:
				if !slices.Equal(got, tc.want) {
					t.Fatalf("subscribe channels = %v, want %v", got, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for the subscribe frame")
			}
		})
	}
}

func TestNotificationClientDispatchesInstallDirective(t *testing.T) {
	directive := validInstallDirective()
	dispatched := make(chan selfupdate.InstallDirective, 1)

	wsURL := startBridgeStub(t, func(ctx context.Context, conn *websocket.Conn, _ int) error {
		if err := expectSubscribeAndReplay(ctx, conn); err != nil {
			return err
		}
		if err := pushInstallDirective(ctx, conn, mustJSON(t, directive)); err != nil {
			return nil
		}
		<-ctx.Done()
		return nil
	})

	client, _ := newTestBridgeClient(t, wsURL, func(got selfupdate.InstallDirective) {
		dispatched <- got
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	select {
	case got := <-dispatched:
		if got != directive {
			t.Fatalf("dispatched directive = %#v, want %#v", got, directive)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the install directive")
	}
}

// TestNotificationClientDropsUnusableInstallDirective pins the trust boundary:
// a directive names a file the launcher resolves on disk, so anything that
// fails validation must never reach the handler.
func TestNotificationClientDropsUnusableInstallDirective(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantLog string
	}{
		{
			name:    "path traversal filename",
			payload: `{"filename":"..\\evil.exe","sha256":"` + strings.Repeat("ab", 32) + `","version":"0.0.11"}`,
			wantLog: "ignore invalid install directive",
		},
		{
			name:    "parent directory filename",
			payload: `{"filename":"../evil.exe","sha256":"` + strings.Repeat("ab", 32) + `","version":"0.0.11"}`,
			wantLog: "ignore invalid install directive",
		},
		{
			name:    "non-hex digest",
			payload: `{"filename":"agent-overflow-wsl-amd64.exe","sha256":"` + strings.Repeat("zz", 32) + `","version":"0.0.11"}`,
			wantLog: "ignore invalid install directive",
		},
		{
			name:    "short digest",
			payload: `{"filename":"agent-overflow-wsl-amd64.exe","sha256":"abcd","version":"0.0.11"}`,
			wantLog: "ignore invalid install directive",
		},
		{
			name:    "junk json",
			payload: `{"filename":`,
			wantLog: "ignore malformed install directive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dispatched := make(chan selfupdate.InstallDirective, 1)
			logs := &bridgeLog{}
			client, err := NewNotificationClient(NotificationClientConfig{
				WSURL:               "ws://127.0.0.1/ws",
				Token:               "test-token",
				Present:             func(notify.Send) error { return nil },
				Logf:                logs.logf,
				HandleUpdateInstall: func(got selfupdate.InstallDirective) { dispatched <- got },
			})
			if err != nil {
				t.Fatalf("NewNotificationClient: %v", err)
			}

			if err := client.handleEvent(notificationEvent{
				Channel: selfupdate.ChannelInstall,
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
			default:
			}
		})
	}
}

// TestNotificationClientInstallEventDoesNotDisturbSendCursor pins that the
// ephemeral install channel stays out of the replay bookkeeping: its sequence
// numbers are independent of notification:send's, so folding them into the
// cursor would fabricate gaps.
func TestNotificationClientInstallEventDoesNotDisturbSendCursor(t *testing.T) {
	logs := &bridgeLog{}
	client, err := NewNotificationClient(NotificationClientConfig{
		WSURL:               "ws://127.0.0.1/ws",
		Token:               "test-token",
		Present:             func(notify.Send) error { return nil },
		Logf:                logs.logf,
		HandleUpdateInstall: func(selfupdate.InstallDirective) {},
	})
	if err != nil {
		t.Fatalf("NewNotificationClient: %v", err)
	}
	client.lastSeq = 4

	payload, err := json.Marshal(validInstallDirective())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.handleEvent(notificationEvent{
		Channel: selfupdate.ChannelInstall,
		Seq:     99,
		Data:    payload,
	}); err != nil {
		t.Fatalf("handleEvent = %v, want nil", err)
	}
	if client.lastSeq != 4 {
		t.Fatalf("last notification sequence = %d, want 4 untouched", client.lastSeq)
	}
}

func TestNotificationClientReportsUpdateInstallStatus(t *testing.T) {
	rpcs := make(chan notificationClientFrame, 2)
	wsURL := startBridgeStub(t, func(ctx context.Context, conn *websocket.Conn, _ int) error {
		if err := expectSubscribeAndReplay(ctx, conn); err != nil {
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
				// Second call: the backend rejects the report.
				response.Error = &notificationFrameError{Code: "method_error", Message: "backend refused"}
			}
			if err := writeServerFrame(ctx, conn, response); err != nil {
				return nil
			}
		}
	})

	client, _ := newTestBridgeClient(t, wsURL, func(selfupdate.InstallDirective) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer callCancel()
	if err := client.ReportUpdateInstallStatus(callCtx, selfupdate.StatusProceeding, "0.0.11", ""); err != nil {
		t.Fatalf("ReportUpdateInstallStatus: %v", err)
	}
	assertStatusRPC(t, rpcs, selfupdate.StatusProceeding, "0.0.11", "")

	err := client.ReportUpdateInstallStatus(callCtx, selfupdate.StatusFailed, "0.0.11", "stat staged update: no such file")
	if err == nil {
		t.Fatal("ReportUpdateInstallStatus = nil, want the backend's rejection")
	}
	var refused *RPCRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v (%T), want a *RPCRefusedError", err, err)
	}
	if refused.Method != selfupdate.RPCReportStatus || refused.Code != "method_error" || refused.Message != "backend refused" {
		t.Fatalf("refusal = %#v, want method %q / code method_error / message \"backend refused\"", refused, selfupdate.RPCReportStatus)
	}
	if !strings.Contains(err.Error(), selfupdate.RPCReportStatus) || !strings.Contains(err.Error(), "backend refused") {
		t.Fatalf("error text = %q, want it to name %s and the backend message", err, selfupdate.RPCReportStatus)
	}
	if got := ClassifyInstallAck(err); got != InstallAckRefused {
		t.Fatalf("ClassifyInstallAck = %v, want %v", got, InstallAckRefused)
	}
	assertStatusRPC(t, rpcs, selfupdate.StatusFailed, "0.0.11", "stat staged update: no such file")
}

// TestNotificationClientReportUpdateInstallStatusBoundedWhileDisconnected pins
// the report's end-to-end bound: with the bridge down, it must fail fast as a
// plain (non-refused) error — which ClassifyInstallAck reads as undelivered —
// instead of riding the caller's context into an indefinite wait for a
// reconnect. Blocking there would hold the launcher's install guard for as
// long as the bridge stays down, and a report finally delivered after the
// backend's ACK deadline would come back as a refusal that aborts a swap the
// user asked for.
func TestNotificationClientReportUpdateInstallStatusBoundedWhileDisconnected(t *testing.T) {
	client, _ := newTestBridgeClient(t, "ws://127.0.0.1:1/ws", func(selfupdate.InstallDirective) {})
	client.rpcTimeout = 50 * time.Millisecond
	// No Run loop: the bridge never connects.

	start := time.Now()
	err := client.ReportUpdateInstallStatus(context.Background(), selfupdate.StatusProceeding, "0.0.11", "")
	if err == nil {
		t.Fatal("ReportUpdateInstallStatus = nil, want a timeout error")
	}
	var refused *RPCRefusedError
	if errors.As(err, &refused) {
		t.Fatalf("error = %v, want a plain error so the launcher classifies it undelivered", err)
	}
	if got := ClassifyInstallAck(err); got != InstallAckUndelivered {
		t.Fatalf("ClassifyInstallAck = %v, want %v", got, InstallAckUndelivered)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("report took %v with the bridge down, want it bounded by the RPC timeout", elapsed)
	}
}

// TestNotificationClientResubscribesInstallChannelAfterReconnect covers a
// directive emitted while the bridge is down: the launcher only sees it because
// the next connection re-subscribes to the install channel, exactly like the
// first one did.
func TestNotificationClientResubscribesInstallChannelAfterReconnect(t *testing.T) {
	directive := validInstallDirective()
	dispatched := make(chan selfupdate.InstallDirective, 1)
	secondSubscribe := make(chan []string, 1)

	wsURL := startBridgeStub(t, func(ctx context.Context, conn *websocket.Conn, connection int) error {
		frame, err := readClientFrame(ctx, conn)
		if err != nil {
			return nil
		}
		if frame.Type != "subscribe" {
			return fmt.Errorf("first frame type = %q, want subscribe", frame.Type)
		}
		if connection == 1 {
			// Drop the connection before the backend ever emits: the directive
			// exists only after the client has reconnected.
			return nil
		}
		secondSubscribe <- frame.Channels
		if _, err := readClientFrame(ctx, conn); err != nil {
			return nil
		}
		if err := writeServerFrame(ctx, conn, notificationServerFrame{Type: "replay"}); err != nil {
			return nil
		}
		if err := pushInstallDirective(ctx, conn, mustJSON(t, directive)); err != nil {
			return nil
		}
		<-ctx.Done()
		return nil
	})

	client, _ := newTestBridgeClient(t, wsURL, func(got selfupdate.InstallDirective) {
		dispatched <- got
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	select {
	case got := <-secondSubscribe:
		want := []string{notify.SendChannel, selfupdate.ChannelInstall}
		if !slices.Equal(got, want) {
			t.Fatalf("reconnect subscribe channels = %v, want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the reconnect subscribe frame")
	}
	select {
	case got := <-dispatched:
		if got != directive {
			t.Fatalf("dispatched directive = %#v, want %#v", got, directive)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the post-reconnect install directive")
	}
}

// --- stub bridge server ---

// startBridgeStub serves the launcher's slice of the transport wire: token
// check, websocket upgrade, then one script invocation per accepted
// connection. A script returns nil for expected disconnects and an error only
// for assertion failures, which fail the test at cleanup.
func startBridgeStub(t *testing.T, script func(ctx context.Context, conn *websocket.Conn, connection int) error) string {
	t.Helper()
	var connections atomic.Int32
	scriptErrors := make(chan error, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "test-token" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			scriptErrors <- err
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		if err := script(r.Context(), conn, int(connections.Add(1))); err != nil {
			scriptErrors <- err
		}
	}))
	t.Cleanup(func() {
		server.Close()
		for {
			select {
			case err := <-scriptErrors:
				t.Errorf("bridge stub: %v", err)
			default:
				return
			}
		}
	})
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func newTestBridgeClient(t *testing.T, wsURL string, handleInstall func(selfupdate.InstallDirective)) (*NotificationClient, *bridgeLog) {
	t.Helper()
	logs := &bridgeLog{}
	client, err := NewNotificationClient(NotificationClientConfig{
		WSURL:               wsURL,
		Token:               "test-token",
		Present:             func(notify.Send) error { return nil },
		Logf:                logs.logf,
		HandleUpdateInstall: handleInstall,
		MinBackoff:          time.Millisecond,
		MaxBackoff:          5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewNotificationClient: %v", err)
	}
	return client, logs
}

func readClientFrame(ctx context.Context, conn *websocket.Conn) (notificationClientFrame, error) {
	_, raw, err := conn.Read(ctx)
	if err != nil {
		return notificationClientFrame{}, err
	}
	var frame notificationClientFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		return notificationClientFrame{}, err
	}
	return frame, nil
}

func writeServerFrame(ctx context.Context, conn *websocket.Conn, frame notificationServerFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, payload)
}

// expectSubscribeAndReplay consumes the client's two opening frames, asserts
// the install channel is in the subscription, and completes the replay so the
// client leaves its buffering state.
func expectSubscribeAndReplay(ctx context.Context, conn *websocket.Conn) error {
	subscribe, err := readClientFrame(ctx, conn)
	if err != nil {
		return err
	}
	if subscribe.Type != "subscribe" {
		return fmt.Errorf("first frame type = %q, want subscribe", subscribe.Type)
	}
	if !slices.Contains(subscribe.Channels, selfupdate.ChannelInstall) {
		return fmt.Errorf("subscribe channels = %v, want %s included", subscribe.Channels, selfupdate.ChannelInstall)
	}
	replay, err := readClientFrame(ctx, conn)
	if err != nil {
		return err
	}
	if replay.Type != "replay" {
		return fmt.Errorf("second frame type = %q, want replay", replay.Type)
	}
	if _, tracked := replay.LastSeqByChannel[selfupdate.ChannelInstall]; tracked {
		return fmt.Errorf("replay request tracks %s; the channel is ephemeral and must not be replayed", selfupdate.ChannelInstall)
	}
	return writeServerFrame(ctx, conn, notificationServerFrame{Type: "replay"})
}

func pushInstallDirective(ctx context.Context, conn *websocket.Conn, payload json.RawMessage) error {
	return writeServerFrame(ctx, conn, notificationServerFrame{
		Type: "event",
		notificationEvent: notificationEvent{
			Channel: selfupdate.ChannelInstall,
			Seq:     1,
			Data:    payload,
		},
	})
}

func assertStatusRPC(t *testing.T, rpcs <-chan notificationClientFrame, wantStage, wantVersion, wantMessage string) {
	t.Helper()
	select {
	case frame := <-rpcs:
		if frame.Method != selfupdate.RPCReportStatus {
			t.Fatalf("rpc method = %q, want %q", frame.Method, selfupdate.RPCReportStatus)
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
		want := []string{wantStage, wantVersion, wantMessage}
		if !slices.Equal(got, want) {
			t.Fatalf("rpc params = %v, want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the install-status RPC")
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	return payload
}

// bridgeLog captures the client's diagnostic output without routing it through
// *testing.T — the client's goroutines outlive the test body, and t.Logf after
// the test returns panics.
type bridgeLog struct {
	mu       sync.Mutex
	captured []string
}

func (l *bridgeLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.captured = append(l.captured, fmt.Sprintf(format, args...))
}

func (l *bridgeLog) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.captured)
}

func (l *bridgeLog) contains(substr string) bool {
	for _, line := range l.lines() {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}
