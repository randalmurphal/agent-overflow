package triage

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// Live 2.1.284 captures of each queued-message consumption path
// (claude-wire.md §"Frame order per consumption path"), replayed through the
// real parser and router with the delivery hook. Each message is registered
// the way the app dispatched it, just before the CLI's `queued` ack for its
// uuid: the app registers a send before it writes it.

const captureDir = "../../docs/references/fixtures/claude/"

// captureSend is how the app dispatched one message of a capture.
type captureSend struct {
	uuid string
	// tag names the request the message carries (`thread-request:<tag>`).
	tag string
	// kind: "direct" opens its own turn from idle; "deferred" was flushed with
	// no turn running and reserved turn `turn`; "quiet" was flushed into
	// running turn `turn`, its response registered at turn+1.
	kind string
	turn int
}

// delivered is one delivery the hook reported.
type delivered struct {
	turn   int
	own    bool
	rebind bool
}

// replayDeliveryCapture feeds a capture through the parser and router and
// returns every delivery the hook reported, by tag, in order.
func replayDeliveryCapture(t *testing.T, fixture string, sends []captureSend) (*store.Store, map[string][]delivered) {
	t.Helper()
	data, err := os.ReadFile(captureDir + fixture)
	if err != nil {
		t.Fatalf("read %s: %v", fixture, err)
	}
	const threadID = "t-capture"
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, threadID)
	log := &deliveryLog{}
	router.SetSendDeliveredHook(log.record)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	texts := captureTexts(t, lines)
	byUUID := make(map[string]captureSend, len(sends))
	for _, send := range sends {
		byUUID[send.uuid] = send
	}
	register := func(send captureSend) {
		text, ok := texts[send.uuid]
		if !ok {
			t.Fatalf("%s: no echo carries %s's own text", fixture, send.tag)
		}
		digest, err := claude.UserMessageBlockDigest(text, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		meta, err := usermessage.MarshalMeta(usermessage.Meta{SendID: "thread-request:" + send.tag})
		if err != nil {
			t.Fatal(err)
		}
		expect := PendingSendExpectation{ProviderItemID: send.uuid, ContentBlockDigest: digest}
		now := time.Now().UnixMilli()
		row := store.Item{
			ID: fmt.Sprintf("user:%d:flush:%s", send.turn, send.tag), ThreadID: threadID, TurnIndex: send.turn,
			Kind: "user_text", Role: "user", Status: "completed", Summary: text, Meta: meta, CreatedAt: now, UpdatedAt: now,
		}
		switch send.kind {
		case "direct":
			router.RegisterPendingSendWithExpectation(threadID, fmt.Sprintf("user:%d:%s", send.turn, send.tag), send.turn, expect)
		case "deferred":
			router.RegisterPendingFlushSendWithExpectation(threadID, "queue:"+send.tag, row, now, expect)
		case "quiet":
			if err := router.PersistAndRegisterPendingQuietFlushSendWithExpectation(threadID, "queue:"+send.tag, row, send.turn+1, now, expect); err != nil {
				t.Fatalf("persist %s: %v", send.tag, err)
			}
		default:
			t.Fatalf("unknown send kind %q", send.kind)
		}
	}

	parser := claude.NewParser()
	base := time.Now()
	for i, line := range lines {
		var frame struct {
			Type        string `json:"type"`
			State       string `json:"state"`
			CommandUUID string `json:"command_uuid"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if frame.Type == "command_lifecycle" && frame.State == "queued" {
			send, ok := byUUID[frame.CommandUUID]
			if !ok {
				t.Fatalf("%s line %d: queued %s, which no send names", fixture, i+1, frame.CommandUUID)
			}
			register(send)
		}
		events, err := parser.ParseLine(threadID, []byte(line))
		if err != nil {
			t.Fatalf("line %d: parse: %v", i+1, err)
		}
		for _, evt := range events {
			evt.ThreadID = threadID
			evt.Timestamp = base.Add(time.Duration(i) * 100 * time.Millisecond)
			if err := router.Handle(evt); err != nil {
				t.Fatalf("line %d: handle %s: %v", i+1, evt.Kind, err)
			}
			router.WaitForPendingSettles()
		}
	}

	got := make(map[string][]delivered)
	for _, delivery := range log.snapshot() {
		for _, id := range delivery.SendIDs {
			tag := strings.TrimPrefix(id, "thread-request:")
			got[tag] = append(got[tag], delivered{turn: delivery.TurnIndex, own: delivery.OwnTurn, rebind: delivery.Rebind})
		}
	}
	return st, got
}

// captureTexts maps each uuid to the text of its own message: the single
// block of its first echo. A survivor echo carries every member's blocks, so
// its own text is its last block.
func captureTexts(t *testing.T, lines []string) map[string]string {
	t.Helper()
	texts := make(map[string]string)
	for _, line := range lines {
		var echo struct {
			Type     string `json:"type"`
			IsReplay bool   `json:"isReplay"`
			UUID     string `json:"uuid"`
			Message  struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &echo) != nil || echo.Type != "user" || !echo.IsReplay || len(echo.Message.Content) == 0 {
			continue
		}
		if _, seen := texts[echo.UUID]; !seen {
			texts[echo.UUID] = echo.Message.Content[len(echo.Message.Content)-1].Text
		}
	}
	return texts
}

func assertDeliveries(t *testing.T, got map[string][]delivered, want map[string][]delivered) {
	t.Helper()
	for tag, deliveries := range want {
		if !slices.Equal(got[tag], deliveries) {
			t.Errorf("%s deliveries = %+v, want %+v (all: %+v)", tag, got[tag], deliveries, got)
		}
	}
}

func assertTurnEnded(t *testing.T, st *store.Store, turnIndex int) {
	t.Helper()
	turn, found, err := st.GetTurnByThreadIndex("t-capture", turnIndex)
	if err != nil || !found || turn.CompletedAt == nil {
		t.Errorf("turn %d = %+v found=%v err=%v, want it ended", turnIndex, turn, found, err)
	}
}

// A message dispatched during a task-notification cascade round and drained
// mid-loop: its echo, then its `started`. The echo opens the message's own
// turn and delivers it there.
func TestReplayQueuedMidloopAfterCascade20261015(t *testing.T) {
	st, got := replayDeliveryCapture(t, "queued_midloop_after_cascade_20261015.ndjson", []captureSend{
		{uuid: "4d8ee9cf-7060-4c57-a25f-ef702b75601f", tag: "t0", kind: "direct", turn: 0},
		{uuid: "7476959b-a610-4616-af65-52549e482621", tag: "m", kind: "deferred", turn: 1},
	})
	assertDeliveries(t, got, map[string][]delivered{"m": {{turn: 1, own: true}}})
	assertTurnEnded(t, st, 1)
}

// A from idle, B dispatched before A's turn opened in AO and drained mid-loop
// into A's round: B's echo opens B's own turn before its `started`.
func TestReplayQueuedDeferredPairMidloop20261015(t *testing.T) {
	st, got := replayDeliveryCapture(t, "queued_deferred_pair_midloop_20261015.ndjson", []captureSend{
		{uuid: "6bb951a6-7235-4250-9110-6e3d421b2c8b", tag: "warmup", kind: "direct", turn: 0},
		{uuid: "a0078c51-a382-4130-8f1f-6069e0681b56", tag: "a", kind: "deferred", turn: 1},
		{uuid: "53e50cce-036f-4dab-8b02-0fd765e6bfa6", tag: "b", kind: "deferred", turn: 2},
	})
	assertDeliveries(t, got, map[string][]delivered{
		"a": {{turn: 1, own: true}},
		"b": {{turn: 2, own: true}},
	})
	assertTurnEnded(t, st, 1)
	assertTurnEnded(t, st, 2)
}

// A and B flushed into a running turn, held and merged at its boundary:
// merged-away echo A, `started` A and B, init, survivor echo B. Both are
// delivered into the turn the init opened, their own.
func TestReplayQueuedBoundaryMerge20261015(t *testing.T) {
	st, got := replayDeliveryCapture(t, "queued_boundary_merge_20261015.ndjson", []captureSend{
		{uuid: "19697499-15f9-4e06-9183-017d27e1b2d0", tag: "t0", kind: "direct", turn: 0},
		{uuid: "4f00b9e4-ec4a-48dc-8509-2edbfe68f0e7", tag: "a", kind: "quiet", turn: 0},
		{uuid: "80e69ef4-030f-4a31-899a-05fe7f946c1f", tag: "b", kind: "quiet", turn: 0},
	})
	assertDeliveries(t, got, map[string][]delivered{
		"a": {{turn: 1, own: true}},
		"b": {{turn: 1, own: true}},
	})
	assertTurnEnded(t, st, 1)
}

// A and B dispatched during a cascade round, after turn 0's result, so each
// reserved its own turn; merged at the round's end. The merged-away echo
// opens A's turn, the fold empties it and moves A to the survivor's turn.
func TestReplayQueuedBoundaryMergeAfterCascade20261015(t *testing.T) {
	st, got := replayDeliveryCapture(t, "queued_boundary_merge_after_cascade_20261015.ndjson", []captureSend{
		{uuid: "b4fb2589-440d-4a2d-beb6-3ff215000e16", tag: "t0", kind: "direct", turn: 0},
		{uuid: "61ad39d2-c20a-4285-899e-371a7bb6cf33", tag: "a", kind: "deferred", turn: 1},
		{uuid: "2dbdd36e-6541-43dc-b6e5-0afed03a4f8b", tag: "b", kind: "deferred", turn: 2},
	})
	assertDeliveries(t, got, map[string][]delivered{
		"a": {{turn: 1, own: true}, {turn: 2, own: true, rebind: true}},
		"b": {{turn: 2, own: true}},
	})
	assertTurnEnded(t, st, 1)
	assertTurnEnded(t, st, 2)
}

// A and B written together at an idle session run as two turns, each
// delivered into its own.
func TestReplayQueuedIdlePairSplit20261015(t *testing.T) {
	st, got := replayDeliveryCapture(t, "queued_idle_pair_split_20261015.ndjson", []captureSend{
		{uuid: "13f0d581-32b2-44ef-b813-d35deabbec3b", tag: "warmup", kind: "direct", turn: 0},
		{uuid: "d37c454d-c5f3-409d-9f05-af4dc132b14f", tag: "a", kind: "deferred", turn: 1},
		{uuid: "9145db28-21c4-4a81-b7d4-6de4ab93d0ce", tag: "b", kind: "deferred", turn: 2},
	})
	assertDeliveries(t, got, map[string][]delivered{
		"a": {{turn: 1, own: true}},
		"b": {{turn: 2, own: true}},
	})
	assertTurnEnded(t, st, 1)
	assertTurnEnded(t, st, 2)
}
