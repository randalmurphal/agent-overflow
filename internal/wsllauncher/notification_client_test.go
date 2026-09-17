package wsllauncher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/notify"

	"github.com/coder/websocket"
)

func TestNotificationClientReconnectsReplaysAndPostsActivation(t *testing.T) {
	target := notify.Target{Kind: "thread", ThreadID: "thread-123"}
	presented := make(chan notify.Send, 2)
	activation := make(chan notify.Target, 1)
	serverErrors := make(chan error, 4)
	var connections atomic.Int32
	var replayMu sync.Mutex
	var replaySeqs []uint64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "test-token" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		connection := connections.Add(1)

		_, raw, err := conn.Read(r.Context())
		if err != nil {
			serverErrors <- err
			return
		}
		var subscribe notificationClientFrame
		if err := json.Unmarshal(raw, &subscribe); err != nil {
			serverErrors <- err
			return
		}
		if subscribe.Type != "subscribe" || len(subscribe.Channels) != 1 || subscribe.Channels[0] != notify.SendChannel {
			serverErrors <- &testNotificationClientError{"unexpected notification subscription"}
			return
		}
		_, raw, err = conn.Read(r.Context())
		if err != nil {
			serverErrors <- err
			return
		}
		var replay notificationClientFrame
		if err := json.Unmarshal(raw, &replay); err != nil {
			serverErrors <- err
			return
		}
		replayMu.Lock()
		replaySeqs = append(replaySeqs, replay.LastSeqByChannel[notify.SendChannel])
		replayMu.Unlock()

		sequence := uint64(connection)
		payload, _ := json.Marshal(notify.Send{
			ID:     "notification-" + string(rune('0'+connection)),
			Kind:   notify.KindTurnComplete,
			Title:  "Ready",
			Body:   "Open it",
			Target: target,
		})
		frame, _ := json.Marshal(notificationServerFrame{
			notificationEvent: notificationEvent{
				Channel: notify.SendChannel,
				Seq:     sequence,
				Data:    payload,
			},
			Type: "event",
		})
		if err := conn.Write(r.Context(), websocket.MessageText, frame); err != nil {
			serverErrors <- err
			return
		}
		replayComplete, _ := json.Marshal(notificationServerFrame{Type: "replay"})
		if err := conn.Write(r.Context(), websocket.MessageText, replayComplete); err != nil {
			serverErrors <- err
			return
		}
		if connection == 1 {
			return
		}

		_, raw, err = conn.Read(r.Context())
		if err != nil {
			serverErrors <- err
			return
		}
		var rpc notificationClientFrame
		if err := json.Unmarshal(raw, &rpc); err != nil {
			serverErrors <- err
			return
		}
		if rpc.Type != "rpc" || rpc.Method != "NotificationActivated" || len(rpc.Params) != 1 {
			serverErrors <- &testNotificationClientError{"unexpected activation RPC"}
			return
		}
		var gotTarget notify.Target
		if err := json.Unmarshal(rpc.Params[0], &gotTarget); err != nil {
			serverErrors <- err
			return
		}
		activation <- gotTarget
		response, _ := json.Marshal(notificationServerFrame{Type: "rpc", ID: rpc.ID})
		if err := conn.Write(r.Context(), websocket.MessageText, response); err != nil {
			serverErrors <- err
		}
	}))
	defer server.Close()

	client, err := NewNotificationClient(NotificationClientConfig{
		WSURL:      "ws" + strings.TrimPrefix(server.URL, "http"),
		Token:      "test-token",
		Present:    func(send notify.Send) error { presented <- send; return nil },
		MinBackoff: time.Millisecond,
		MaxBackoff: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewNotificationClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	for want := 1; want <= 2; want++ {
		select {
		case got := <-presented:
			if got.Target != target {
				t.Fatalf("presented target = %#v, want %#v", got.Target, target)
			}
		case err := <-serverErrors:
			t.Fatalf("stub server: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for presentation %d", want)
		}
	}

	activateCtx, activateCancel := context.WithTimeout(context.Background(), time.Second)
	defer activateCancel()
	if err := client.Activate(activateCtx, target); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	select {
	case got := <-activation:
		if got != target {
			t.Fatalf("activation target = %#v, want %#v", got, target)
		}
	case <-time.After(time.Second):
		t.Fatal("stub server did not receive activation RPC")
	}

	replayMu.Lock()
	defer replayMu.Unlock()
	if len(replaySeqs) < 2 || replaySeqs[0] != 0 || replaySeqs[1] != 1 {
		t.Fatalf("replay sequences = %v, want prefix [0 1]", replaySeqs)
	}
}

func TestNotificationClientSequenceGapKeepsReplayCheckpoint(t *testing.T) {
	client, err := NewNotificationClient(NotificationClientConfig{
		WSURL:   "ws://127.0.0.1/ws",
		Token:   "test-token",
		Present: func(notify.Send) error { return nil },
		Logf:    t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.lastSeq = 4
	err = client.handleEvent(notificationEvent{
		Channel: notify.SendChannel,
		Seq:     6,
		Data:    json.RawMessage(`null`),
	})
	if err == nil || !strings.Contains(err.Error(), "sequence gap") {
		t.Fatalf("handleEvent error = %v, want sequence gap", err)
	}
	if client.lastSeq != 4 {
		t.Fatalf("last sequence = %d, want checkpoint 4 preserved", client.lastSeq)
	}
}

func TestNotificationClientOrdersLiveEventBehindReplayEvent(t *testing.T) {
	presented := make([]string, 0, 2)
	client, err := NewNotificationClient(NotificationClientConfig{
		WSURL: "ws://127.0.0.1/ws",
		Token: "test-token",
		Present: func(send notify.Send) error {
			presented = append(presented, send.ID)
			return nil
		},
		Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	event := func(seq uint64) notificationEvent {
		payload, marshalErr := json.Marshal(notify.Send{
			ID:     "notification-" + string(rune('0'+seq)),
			Kind:   notify.KindTurnComplete,
			Title:  "Ready",
			Target: notify.Target{Kind: "none"},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return notificationEvent{Channel: notify.SendChannel, Seq: seq, Data: payload}
	}

	if err := client.handleReplayEvents([]notificationEvent{event(2), event(1)}); err != nil {
		t.Fatalf("handleReplayEvents: %v", err)
	}
	if got, want := strings.Join(presented, ","), "notification-1,notification-2"; got != want {
		t.Fatalf("presentation order = %q, want %q", got, want)
	}
}

func TestNotificationBridgeErrorRedactsToken(t *testing.T) {
	token := "secret+/token"
	message := redactNotificationBridgeError(
		&testNotificationClientError{"GET ws://127.0.0.1/ws?token=secret%2B%2Ftoken: refused"},
		token,
	)
	if strings.Contains(message, token) || strings.Contains(message, "secret%2B%2Ftoken") {
		t.Fatalf("redacted error leaked token: %q", message)
	}
}

type testNotificationClientError struct{ message string }

func (e *testNotificationClientError) Error() string { return e.message }

// The banner's sound answer crosses the process boundary INTACT. The backend
// resolves it against its own screen's settings (App.notifyOS) and this side
// only carries it, so a frame that loses `silent` on the way through would
// make every bridged toast play the Windows sound on top of the app's cue.
//
// Raw JSON rather than a marshalled notify.Send: the wire is what the
// launcher decodes, and a round-trip through the Go shape would pass even if
// the field were dropped from the struct tag.
func TestABridgedNotificationCarriesItsSilentAnswer(t *testing.T) {
	presented := make([]notify.Send, 0, 2)
	client, err := NewNotificationClient(NotificationClientConfig{
		WSURL: "ws://127.0.0.1/ws",
		Token: "test-token",
		Present: func(send notify.Send) error {
			presented = append(presented, send)
			return nil
		},
		Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	silent := `{"id":"thread:t1","kind":"turn-complete","title":"Ready","body":"Completed",` +
		`"target":{"kind":"none"},"silent":true}`
	if err := client.handleEvent(notificationEvent{
		Channel: notify.SendChannel, Seq: 1, Data: json.RawMessage(silent),
	}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	// An absent field is the other answer, not a missing one: the banner
	// carries the platform's default sound.
	audible := `{"id":"thread:t2","kind":"turn-complete","title":"Ready","body":"Completed",` +
		`"target":{"kind":"none"}}`
	if err := client.handleEvent(notificationEvent{
		Channel: notify.SendChannel, Seq: 2, Data: json.RawMessage(audible),
	}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}

	if len(presented) != 2 {
		t.Fatalf("presented %d notifications, want 2", len(presented))
	}
	if !presented[0].Silent {
		t.Fatalf("presented[0] = %+v, want Silent", presented[0])
	}
	if presented[1].Silent {
		t.Fatalf("presented[1] = %+v, want the platform sound", presented[1])
	}
}

// A retraction carrying the field is refused by the shared admission check
// rather than presented: there is no banner for a sound to ride on.
func TestABridgedRetractionCarryingSilentIsDropped(t *testing.T) {
	presented := 0
	client, err := NewNotificationClient(NotificationClientConfig{
		WSURL:   "ws://127.0.0.1/ws",
		Token:   "test-token",
		Present: func(notify.Send) error { presented++; return nil },
		Logf:    t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame := `{"id":"thread:t1","kind":"turn-complete","retract":true,"silent":true}`
	if err := client.handleEvent(notificationEvent{
		Channel: notify.SendChannel, Seq: 1, Data: json.RawMessage(frame),
	}); err != nil {
		t.Fatalf("handleEvent = %v, want nil (an invalid payload must not kill the connection)", err)
	}
	if presented != 0 {
		t.Fatalf("presented %d notifications, want none", presented)
	}
}
