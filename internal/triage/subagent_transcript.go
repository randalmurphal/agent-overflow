package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
	claudeimport "agent-overflow/internal/provider/claude/sessionimport"
	"agent-overflow/internal/store"
)

// Subagent transcript backfill (docs/specs/agent-visibility.md, success
// criterion "the transcript completes on the task notification").
//
// An agent launched ASYNC streams its whole sidechain over the wire: 92
// of 92 §E5 launches in AO logs 2026-08-15 → 08-22 have rows after their
// ack. An agent BACKGROUNDED mid-flight streams nothing at all after the
// cut — zero sidechain envelopes; only `system/task_progress` counters
// and its Bash calls' own bookends leak through (claude-wire.md
// §background_tasks). Its work exists in exactly one place: the sidechain
// JSONL that `system/task_notification` names as `output_file`.
//
// New Claude sessions also emit transcript_mirror frames, which the parser
// projects live through the SAME converter and marks on the launch. The
// terminal transcript still reconciles every launch: a mirror is an
// append-only delivery channel, not proof that its final batch arrived.
//
// Why the events are replayed rather than written as rows: see
// internal/triage/AGENTS.md §Subagent transcript backfill. Short version — a
// subagent-scoped row's id is allocated from the Router's live per-scope
// counter, so only the live path can mint an id a later live lookup will
// find.

// backfillSubagentTranscript replays the part of launch's sidechain
// transcript the live stream never delivered, and reports how many
// events it replayed.
//
// Failure is loud. A file that cannot be resolved, or is larger than the
// payload ceiling, is reported to the caller, which stamps it onto the
// notification's output-file state — a silently incomplete agent
// transcript reads exactly like a complete one, and no second signal
// would ever correct it.
func (r *Router) backfillSubagentTranscript(threadID string, launch store.Item, data []byte) (int, error) {
	converted, err := claudeimport.ConvertSubagentTranscriptData(data, launch.ID)
	if err != nil {
		return 0, fmt.Errorf("read subagent transcript: %w", err)
	}
	for _, warning := range converted.Warnings {
		log.Printf("triage: subagent transcript %s (%s): %s", launch.ID, warning.Code, warning.Message)
	}
	if len(converted.Events) == 0 {
		// Not an error: an agent killed before it produced anything
		// leaves a transcript with no convertible rows. Logged because
		// an empty projection and one that was never attempted are
		// otherwise indistinguishable after the fact.
		log.Printf("triage: subagent transcript %s projected no events", launch.ID)
		return 0, nil
	}

	delivered, err := r.subagentDeliveredRows(threadID, launch)
	if err != nil {
		return 0, err
	}

	cut := subagentBackfillCut(converted.Events, delivered)
	replayed := 0
	for i, event := range converted.Events {
		if !replaySubagentEventAt(i, cut, event.ProviderEvent, delivered) {
			continue
		}
		written, err := r.replaySubagentEvent(threadID, launch, event.ProviderEvent)
		if err != nil {
			return replayed, fmt.Errorf("replay %s from subagent transcript: %w", event.Kind, err)
		}
		if written {
			replayed++
		}
	}
	return replayed, nil
}

// replaySubagentEventAt decides whether one projected event is replayed,
// given where the cut fell.
//
// Past the cut, everything is. Before it, one kind still can be: the
// agent's own prompt, and only when the thread has no row for it. The
// prompt is not part of the streamed sequence the cut reasons about —
// an ASYNC agent streams its whole sidechain and never echoes its
// prompt, so a fully-delivered transcript is still missing that one row
// — and letting it decide the cut instead would replay every async
// agent's whole transcript from row zero.
func replaySubagentEventAt(index, cut int, evt provider.ProviderEvent, delivered subagentDeliveredRows) bool {
	if evt.Kind == provider.EventCompactBoundary {
		// The SDK's agent_progress mapper selectively drops the boundary and
		// isCompactSummary marker while continuing to forward later rows. A
		// missing compaction is therefore not evidence that the live stream
		// stopped. Reconcile this exact provider UUID independently of the cut.
		decidable, present := subagentEventDelivered(evt, delivered)
		return decidable && !present
	}
	if index >= cut {
		return true
	}
	if evt.Kind != provider.EventUserText {
		return false
	}
	_, present := subagentEventDelivered(evt, delivered)
	return !present
}

// subagentDeliveredRows is the identity index of everything the live
// stream already wrote for a launch. ONE query: a subagent's rows all
// carry the launch's turn_index (invariant 10), so the turn is a
// superset of the rows a replay could duplicate, and asking per event
// would be one SELECT per transcript line on the common path where
// nothing is missing at all.
type subagentDeliveredRows struct {
	turnIndex int
	// byID is every row of the launch's turn, keyed by row id. Tool
	// calls live here: their row id IS the `tool_use_id`, on both the
	// live path and in the transcript.
	byID map[string]store.Item
	// byProviderItem is the launch's own assistant_text / thinking rows,
	// keyed `<kind>|<provider_item_id>`. Those rows are NOT addressable
	// by id — the id embeds a per-scope sequence number the Router
	// allocates, which the transcript cannot know — so the durable
	// identity is the provider's own `message.id#ordinal`, which the
	// live parser (recoveredBlockItemID) and the importer's converter
	// (nextBlockItemID) spell identically.
	byProviderItem map[string]store.Item
	// openingPromptByProviderItem covers the launch-scoped prompt row. Its
	// item id is derived from the launch so it can exist before Claude reveals
	// the transcript uuid; meta gains that uuid when the real row arrives.
	openingPromptByProviderItem map[string]store.Item
}

func (r *Router) subagentDeliveredRows(threadID string, launch store.Item) (subagentDeliveredRows, error) {
	items, err := r.store.ListTurnItemsSansPayload(threadID, launch.TurnIndex)
	if err != nil {
		return subagentDeliveredRows{}, fmt.Errorf("list turn %d of %s: %w", launch.TurnIndex, threadID, err)
	}
	index := subagentDeliveredRows{
		turnIndex:                   launch.TurnIndex,
		byID:                        make(map[string]store.Item, len(items)),
		byProviderItem:              map[string]store.Item{},
		openingPromptByProviderItem: map[string]store.Item{},
	}
	for _, item := range items {
		index.byID[item.ID] = item
		if item.ParentID != launch.ID {
			continue
		}
		if providerItemID := decodeProviderItemID(item.Meta); providerItemID != "" {
			if item.Kind == itemKindUserText {
				opening, _, _, stateErr := subagentOpeningPromptState(item.Meta)
				if stateErr != nil {
					return subagentDeliveredRows{}, fmt.Errorf("decode opening prompt %s/%s: %w", threadID, item.ID, stateErr)
				}
				if opening {
					index.openingPromptByProviderItem[providerItemID] = item
				}
			} else if item.Kind == itemKindAssistantText || item.Kind == itemKindThinking {
				index.byProviderItem[item.Kind+"|"+providerItemID] = item
			}
		}
	}
	return index, nil
}

func decodeProviderItemID(meta string) string {
	if strings.TrimSpace(meta) == "" {
		return ""
	}
	var decoded struct {
		ProviderItemID string `json:"provider_item_id"`
	}
	if json.Unmarshal([]byte(meta), &decoded) != nil {
		return ""
	}
	return strings.TrimSpace(decoded.ProviderItemID)
}

// subagentBackfillCut finds where the live stream stopped: the index of
// the first event whose row is not already on the thread.
//
// A cut rather than a per-event filter, because that is the shape of the
// wire fact. Backgrounding an agent stops its sidechain at a point;
// everything before that point streamed and everything after it did not.
// Working out the point from the ordinary streamed events that carry a
// durable identity (tool_use ids and `message.id#ordinal`) then carries the
// ones that do not — error rows and slash-command results — along with their
// neighbours. Compaction is the exception: the SDK selectively omits it while
// forwarding later rows, so its provider UUID is reconciled independently and
// never moves the cut.
//
// len(events) when everything is already present, which is the async
// agent's answer and makes the whole backfill a no-op.
func subagentBackfillCut(events []importir.Event, delivered subagentDeliveredRows) int {
	for i := range events {
		if events[i].Kind == provider.EventUserText {
			// The prompt is not evidence about where streaming stopped —
			// see replaySubagentEventAt.
			continue
		}
		if events[i].Kind == provider.EventCompactBoundary {
			// Selective SDK omission, not a sidechain delivery cut. It is
			// reconciled by provider UUID in replaySubagentEventAt.
			continue
		}
		decidable, present := subagentEventDelivered(events[i].ProviderEvent, delivered)
		if !decidable || present {
			continue
		}
		return i
	}
	return len(events)
}

// subagentEventDelivered answers "did the live stream already write this
// event's row?" — and whether the question can be answered at all.
func subagentEventDelivered(evt provider.ProviderEvent, delivered subagentDeliveredRows) (decidable, present bool) {
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return false, false
	}
	switch evt.Kind {
	case provider.EventToolStart:
		_, found := delivered.byID[itemID]
		return true, found
	case provider.EventToolComplete:
		// A launch row the live stream left `running` is NOT delivered.
		// That is precisely the tool that was in flight when the agent
		// stopped streaming, and nothing else will ever settle it: the
		// agent has reached its terminal and its sidechain is closed, so
		// without the replay the card spins forever.
		row, found := delivered.byID[itemID]
		return true, found && row.Status != statusRunning
	case provider.EventTextDelta:
		row, found := delivered.byProviderItem[itemKindAssistantText+"|"+itemID]
		return true, found && row.Status == statusCompleted
	case provider.EventThinking:
		row, found := delivered.byProviderItem[itemKindThinking+"|"+itemID]
		return true, found && row.Status == statusCompleted
	case provider.EventUserText:
		// Opening prompts use the launch-scoped row identity so they can be
		// rendered at launch time. Older and later user-role rows retain the
		// provider-keyed identity for backward compatibility.
		_, found := delivered.openingPromptByProviderItem[itemID]
		if !found {
			_, found = delivered.byID["user:wire:"+itemID]
		}
		return true, found
	case provider.EventCompactBoundary:
		// Compaction rows use the boundary's provider UUID in their durable
		// item id. Unlike sequenced error/notification rows, this is exact
		// identity and can safely prove whether the boundary arrived live.
		if NormalizeProviderCompactionID(itemID) == "" {
			return false, false
		}
		_, found := delivered.byID[CompactionItemID(delivered.turnIndex, itemID, 0)]
		return true, found
	default:
		return false, false
	}
}

// replaySubagentEvent persists ONE transcript event exactly as a live
// one, and reports whether it produced a write.
//
// Text and thinking do not go through handleTextDelta / handleThinking:
// those open a streaming block and wait for a stop the transcript has no
// event for, which would leave the scope's streaming count incremented
// forever and wedge the interrupt queue. persistOrUpdateCompleted*Item
// is the same settle those handlers eventually reach — and the same one
// the live subagent path already uses, since the CLI emits no partial
// stream events for subagent messages — so it is both the correct shape
// and idempotent on a re-delivered block.
//
// Everything else goes through the ordinary dispatch. These are the live
// handlers for the live events; a transcript row that projects to
// EventToolStart is the same fact the wire's tool_use block was.
func (r *Router) replaySubagentEvent(threadID string, launch store.Item, evt provider.ProviderEvent) (bool, error) {
	evt.ThreadID = threadID
	if strings.TrimSpace(evt.ParentToolUseID) == "" {
		evt.ParentToolUseID = launch.ID
	}
	switch evt.Kind {
	case provider.EventTextDelta:
		if evt.Content == "" {
			return false, nil
		}
		return true, r.persistOrUpdateCompletedTextItem(
			threadID, launch.TurnIndex, launch.ID, strings.TrimSpace(evt.ItemID), evt.Content, nil)
	case provider.EventThinking:
		if evt.Content == "" {
			return false, nil
		}
		return true, r.persistOrUpdateCompletedThinkingItem(
			threadID, launch.TurnIndex, launch.ID, strings.TrimSpace(evt.ItemID), evt.Content)
	case provider.EventError:
		evt.Meta = replayedErrorMeta(evt.Meta)
		return true, r.dispatch(evt)
	case provider.EventUserText:
		// The first user row is the task text the CLI handed the agent.
		// A backgrounded agent never echoes it on ordinary stdout. New
		// launches already have a provisional row from their tool input;
		// preserving the converter's opening marker lets handleUserText
		// bind this transcript uuid onto that row without moving it. Later
		// scoped user rows keep their provider-keyed identities.
		if strings.TrimSpace(evt.Content) == "" {
			return false, nil
		}
		uuid := strings.TrimSpace(evt.ItemID)
		if uuid == "" {
			return false, nil
		}
		metaFields := decodeUserTextMeta(evt.Meta)
		if metaFields == nil {
			metaFields = userTextMeta{}
		}
		providerItemID, err := json.Marshal(uuid)
		if err != nil {
			return false, fmt.Errorf("encode subagent prompt provider id %s: %w", uuid, err)
		}
		metaFields["provider_item_id"] = providerItemID
		meta, err := json.Marshal(metaFields)
		if err != nil {
			return false, fmt.Errorf("encode subagent prompt meta for %s: %w", uuid, err)
		}
		evt.Meta = meta
		return true, r.dispatch(evt)
	case provider.EventToolStart, provider.EventToolComplete,
		provider.EventNotification, provider.EventCommandResult,
		provider.EventCompactBoundary:
		// Every one of these lands under the launch: the handlers scope
		// their row by ParentToolUseID and resolve the turn from it, and
		// handleCompaction keeps a scoped boundary off the thread's
		// compacting window and context meter — a subagent's compaction
		// is private to the subagent.
		return true, r.dispatch(evt)
	default:
		// Turn boundaries, usage frames, and anything a future reader
		// grows. A subagent has no turns of its own (invariant 10) and
		// its usage rides the parent's turn, so replaying either would
		// write history the launching turn already owns.
		return false, nil
	}
}

// replayedErrorMeta rewrites a transcript error's meta for replay. The
// error is history: it happened inside a detached agent, after the cut,
// and the CLI did not end the main turn for it (the turn the agent was
// launched in closed at the cut, and the agent ran on alone). The
// converter stamps `fatal:true` because on a session IMPORT a fatal API
// error is the end of the session it reads; dispatched live here it
// would flip every running row in the thread's CURRENT turn to errored
// and run the fatal-error finish, for an error the main agent never saw.
// The enum stays, so the row still persists as `api_error` with its
// actionable copy — under the launch, where the failure belongs.
func replayedErrorMeta(meta json.RawMessage) json.RawMessage {
	if len(meta) == 0 {
		return meta
	}
	var fields map[string]any
	if err := json.Unmarshal(meta, &fields); err != nil || fields == nil {
		return meta
	}
	fields["fatal"] = false
	delete(fields, "expect_turn_complete")
	rewritten, err := json.Marshal(fields)
	if err != nil {
		return meta
	}
	return rewritten
}

// isSubagentTranscriptLaunch reports whether a launch's `output_file` is
// an agent sidechain transcript rather than a command's captured output.
//
// Claude names the same field for both task types: for `local_bash` it
// is stdout/stderr, which the command_output payload path already owns
// and which is not JSONL at all; for `local_agent` it is the agent's
// `subagents/agent-<id>.jsonl`. The tool name is the discriminator, and
// it is the same one the payload builder splits on
// (isCommandOutputLaunch), so the two cannot disagree about which shape
// a file has.
func isSubagentTranscriptLaunch(launch store.Item) bool {
	return launch.Kind == itemKindToolCall &&
		strings.TrimSpace(launch.ToolName) != "" &&
		!isCommandOutputLaunch(launch)
}
