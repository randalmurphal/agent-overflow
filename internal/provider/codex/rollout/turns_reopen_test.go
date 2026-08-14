package rollout

// Regression tests for turn-id reuse within one Parse. Codex writes records
// AFTER a turn settles that still relate to it — a trailing `token_count` or
// `thread_rolled_back` behind a `turn_aborted`, or a `task_complete` racing
// an abort — while `pendingCtx` still names the settled turn. Every path
// here used to re-open a turn under an id the Parse had already used, which
// the import writer then refused as a primary-key collision, hard-failing
// the whole session on FIRST import (found by the corpus smoke: 122 of 1297
// real rollouts).

import (
	"strings"
	"testing"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
)

const (
	turnAbortedLine = `{"timestamp":"2026-08-07T19:07:50.000Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-1","reason":"interrupted","completed_at":1786133870}}`
	tokenCountLine  = `{"timestamp":"2026-08-07T19:07:50.100Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":120,"cached_input_tokens":0,"output_tokens":30,"reasoning_output_tokens":0,"total_tokens":150},"model_context_window":258400}}}`
	rolledBackLine  = `{"timestamp":"2026-08-07T19:07:50.200Z","type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":1}}`
	turnContext2Ln  = `{"timestamp":"2026-08-07T19:07:55.000Z","type":"turn_context","payload":{"turn_id":"turn-2","cwd":"/repo","model":"gpt-5.6-sol","effort":"high"}}`
	taskStarted2Ln  = `{"timestamp":"2026-08-07T19:07:55.100Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2","started_at":1786133875,"model_context_window":258400}}`
	taskComplete2Ln = `{"timestamp":"2026-08-07T19:07:59.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-2","last_agent_message":"done","started_at":1786133875,"completed_at":1786133879}}`
)

// turnStartIDs collects the TurnID of every turn-start event, in order.
func turnStartIDs(events []importir.Event) []string {
	var ids []string
	for _, e := range events {
		if e.Kind == provider.EventTurnStart {
			ids = append(ids, e.TurnID)
		}
	}
	return ids
}

func assertUniqueTurnStartIDs(t *testing.T, events []importir.Event) {
	t.Helper()
	seen := map[string]struct{}{}
	for _, id := range turnStartIDs(events) {
		if _, dup := seen[id]; dup {
			t.Fatalf("turn id %q opened twice in one Parse; turn starts = %v", id, turnStartIDs(events))
		}
		seen[id] = struct{}{}
	}
}

func TestParseTrailingRecordsAfterAbortDoNotReuseTheTurnID(t *testing.T) {
	path := writeRollout(t, testSessionID,
		metaLine, turnContextLine, taskStartedLine, userMsgLine,
		turnAbortedLine, tokenCountLine, rolledBackLine,
		turnContext2Ln, taskStarted2Ln, agentMsgLine, taskComplete2Ln)
	res := parseFixture(t, path)

	assertUniqueTurnStartIDs(t, res.Events)
	ids := turnStartIDs(res.Events)
	if len(ids) != 3 {
		t.Fatalf("turn starts = %v, want [turn-1, synthetic, turn-2]", ids)
	}
	if ids[0] != "turn-1" || ids[2] != "turn-2" {
		t.Fatalf("wire turns lost their ids: %v", ids)
	}
	if !strings.HasPrefix(ids[1], "import-turn:"+testSessionID+":") {
		t.Fatalf("post-abort trailing records reused id %q instead of minting a synthetic turn", ids[1])
	}
	// The synthetic turn says so, and carries the rolled-back notification.
	for _, e := range res.Events {
		if e.Kind == provider.EventTurnStart && e.TurnID == ids[1] {
			if !strings.Contains(string(e.Meta), "import_synthetic_turn") {
				t.Fatalf("post-abort turn is not marked synthetic: %s", e.Meta)
			}
		}
		if e.Kind == provider.EventNotification && strings.Contains(string(e.Meta), "thread_rolled_back") {
			if e.TurnID != ids[1] {
				t.Fatalf("rolled-back notification landed on turn %q, want the synthetic %q", e.TurnID, ids[1])
			}
		}
	}
}

func TestParseSyntheticTurnIDsAreStableWithinAndUniqueAcrossSessions(t *testing.T) {
	const otherSessionID = "019f1111-2222-7333-8444-555555555555"
	firstPath := writeRollout(t, testSessionID, metaLine, userMsgLine)
	otherPath := writeRollout(t, otherSessionID, metaLine, userMsgLine)

	first := parseFixture(t, firstPath)
	firstAgain := parseFixture(t, firstPath)
	other := parseFixture(t, otherPath)
	firstID := turnStartIDs(first.Events)[0]
	if againID := turnStartIDs(firstAgain.Events)[0]; againID != firstID {
		t.Fatalf("same session minted unstable synthetic ids: %q then %q", firstID, againID)
	}
	if otherID := turnStartIDs(other.Events)[0]; otherID == firstID {
		t.Fatalf("different sessions shared synthetic turn id %q", firstID)
	}
	if want := "import-turn:" + testSessionID + ":1"; firstID != want {
		t.Fatalf("synthetic turn id = %q, want %q", firstID, want)
	}
}

func TestParseTaskStartedAdoptsTheSyntheticTurnItsContextOpened(t *testing.T) {
	// Content between a turn_context and its task_started opens a synthetic
	// turn under the context's id; the boundary must adopt it in place, not
	// close and re-open (which claims the id twice in the writer).
	path := writeRollout(t, testSessionID,
		metaLine, turnContextLine, userMsgLine, taskStartedLine,
		agentMsgLine, taskCompleteLn)
	res := parseFixture(t, path)

	assertUniqueTurnStartIDs(t, res.Events)
	ids := turnStartIDs(res.Events)
	if len(ids) != 1 || ids[0] != "turn-1" {
		t.Fatalf("turn starts = %v, want exactly [turn-1]", ids)
	}
	if n := countKind(res.Events, provider.EventTurnComplete); n != 1 {
		t.Fatalf("turn completes = %d, want 1", n)
	}
	// The adopted turn settles on the wire completion, not the synthetic
	// close.
	complete := firstOfKind(t, res.Events, provider.EventTurnComplete)
	if _, ok := complete.TurnComplete.(*provider.WireTurnCompleteMeta); !ok {
		t.Fatalf("adopted turn settled as %T, want wire completion", complete.TurnComplete)
	}
}

func TestParseTaskCompleteAfterAbortDoesNotReopenTheTurn(t *testing.T) {
	taskComplete1Ln := `{"timestamp":"2026-08-07T19:07:51.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1","last_agent_message":"","started_at":1786133866,"completed_at":1786133871}}`
	path := writeRollout(t, testSessionID,
		metaLine, turnContextLine, taskStartedLine, userMsgLine,
		turnAbortedLine, taskComplete1Ln)
	res := parseFixture(t, path)

	assertUniqueTurnStartIDs(t, res.Events)
	if ids := turnStartIDs(res.Events); len(ids) != 1 || ids[0] != "turn-1" {
		t.Fatalf("turn starts = %v, want exactly [turn-1]", ids)
	}
	if n := countKind(res.Events, provider.EventTurnComplete); n != 1 {
		t.Fatalf("turn completes = %d, want 1 (the abort's settle stands)", n)
	}
	complete := firstOfKind(t, res.Events, provider.EventTurnComplete)
	wire, ok := complete.TurnComplete.(*provider.WireTurnCompleteMeta)
	if !ok || !wire.Aborted {
		t.Fatalf("turn settled as %T aborted=%v, want the abort", complete.TurnComplete, ok && wire.Aborted)
	}
}
