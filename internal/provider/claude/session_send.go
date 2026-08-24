package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"

	"github.com/google/uuid"
)

// Send sends a user message. The message is written as a JSON object to stdin.
// There is intentionally no idle watchdog on the response channel — Claude
// may legitimately sit silent for long periods while waiting on a pending
// can_use_tool prompt or thinking through a hard request. The user-facing
// Stop button is the authoritative way to abort.
func (s *Session) Send(ctx context.Context, content string, opts provider.SendOptions) error {
	// Validate the client-supplied message id before any side effect
	// (permission-mode control_request, stdin write) so a malformed id
	// fails the send loudly instead of poisoning the session JSONL with a
	// uuid the revert path can never match. Reject non-canonical forms too:
	// the caller (app_send.go) stamps this exact string on the user row and
	// the message anchor, and the revert path matches the JSONL `uuid`
	// against that stored string byte-for-byte. Normalizing here instead
	// would desync the envelope from the pre-stamped row; sending a
	// non-canonical id as-is would bet on the CLI never canonicalizing.
	// Requiring canonical input keeps row, anchor, envelope, and the
	// echoed JSONL uuid identical, and turns the parsed value into a real
	// check rather than a discarded result.
	if opts.UserMessageUUID != "" {
		parsed, err := uuid.Parse(opts.UserMessageUUID)
		if err != nil {
			return fmt.Errorf("claude: invalid user message uuid %q: %w", opts.UserMessageUUID, err)
		}
		if parsed.String() != opts.UserMessageUUID {
			return fmt.Errorf("claude: user message uuid %q is not in canonical form (want %q)", opts.UserMessageUUID, parsed.String())
		}
	}
	interactionMode := opts.InteractionMode
	if interactionMode == "" {
		interactionMode = s.interactionMode
	}
	if err := s.setPermissionMode(ctx, s.desiredPermissionModeForTurn(interactionMode)); err != nil {
		return err
	}
	attachments := opts.Attachments

	message := map[string]any{
		"role": "user",
	}
	blocks, err := buildUserMessageBlocks(content, attachments, opts.GuardClaudeSlashCommand)
	if err != nil {
		return err
	}
	message["content"] = blocks

	s.recordExpectedReplayParent(opts.UserMessageUUID)
	// Recorded BEFORE the stdin write: the CLI's command_lifecycle bracket
	// for this uuid can reach the read loop before Send returns, and an
	// unrecorded uuid would classify this app's own send as a turn some
	// peer session started (session_peer.go).
	s.noteIssuedCommandUUID(opts.UserMessageUUID)
	// Native slash-command correlation must be present before the write for
	// the same reason as issuedCommands: lifecycle and mirror frames can race
	// Send's return.
	s.directCommands.note(opts.UserMessageUUID, content, opts)
	// Same timing, same reason: the command's own `<synthetic>` output can
	// reach the read loop before Send returns, and an unrecorded uuid would
	// let AO's bookkeeping land in the user's transcript
	// (command_result_suppression.go).
	s.noteSuppressedCommandResult(opts.UserMessageUUID, content, opts)

	msg := map[string]any{
		"type":    "user",
		"message": message,
	}
	// Stamp the client-minted message id as the envelope's top-level
	// `uuid`. Verified behaviour (claude v2.1.150, --input-format
	// stream-json persistent mode — AO's exact flags): the CLI persists
	// this exact value as the user entry's `uuid` in its session JSONL and
	// echoes it back on the --replay-user-messages envelope, assigning only
	// `parentUuid` itself. AO relies on this so a revert can slice the
	// transcript by a uuid it knew at send time, before the replay echo
	// arrives. This is an undocumented binary contract — if the CLI version
	// moves and revert-by-uuid starts falling back to the ordinal walk,
	// re-spike per docs/references/spike-policy.md before assuming this
	// still holds. Verified behaviour + write-timing data are captured in
	// docs/references/claude-wire.md §"Outbound user message".
	if opts.UserMessageUUID != "" {
		msg["uuid"] = opts.UserMessageUUID
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("claude: marshal user message: %w", err)
	}
	if err := s.proc.WriteLine(data); err != nil {
		s.clearExpectedReplayParent(opts.UserMessageUUID)
		// The uuid never reached stdin, so no command_lifecycle bracket will
		// ever arrive to consume it. Leaving it in the ledger would leak one
		// entry per failed write, and the ledger is capped: 256 failures
		// (a wedged CLI whose stdin pipe is broken retries forever) latch
		// `overflow` and every peer turn for the rest of the session reads
		// as local. Releasing here is safe precisely because the write
		// failed — nothing on the wire can still claim this uuid.
		s.releaseIssuedCommandUUID(opts.UserMessageUUID)
		s.directCommands.release(opts.UserMessageUUID)
		s.releaseSuppressedCommandResult(opts.UserMessageUUID)
		return err
	}
	return nil
}

// Interrupt aborts the current turn by sending a control_request with
// subtype "interrupt" and waiting for the CLI's control_response. Per
// claude-wire.md §control_request, the CLI's interrupt handler stops
// the model and reaps in-flight foreground tool subprocesses;
// backgrounded tasks (Bash run_in_background:true, Task subagents)
// survive by design and are stopped individually via stop_task.
//
// If the CLI never acks (timeout or caller-context cancellation), the
// error surfaces to the caller — the failure is the CLI's to fix
// (every Anthropic SDK uses the same control_request primitive). We
// deliberately do NOT escalate to a process kill here: a kill would
// take down backgrounded tasks too, inverting the documented
// foreground-only behaviour and silently masking a Claude Code bug.
func (s *Session) Interrupt(ctx context.Context) error {
	res, err := s.sendControlRequest(ctx, "interrupt", map[string]any{
		"subtype": "interrupt",
	})
	if err != nil {
		return err
	}
	return interpretControlResponse(res, "interrupt")
}

// StopTask kills a backgrounded Claude task (Bash with
// run_in_background:true or a Task subagent) by sending a `stop_task`
// control_request and awaiting the matching control_response. The
// `task_id` argument is the id the CLI emitted on `system/task_started`
// — the Claude control protocol accepts the same id for both task
// types.
//
// On success the CLI replies with
// `{"type":"control_response","response":{"subtype":"success", ...}}`
// and fires a follow-up `system/task_updated` with
// `patch.status:"killed"` on the normal event stream (routed through
// triage just like a natural terminal). On error the response carries
// `subtype:"error"` with a human-readable message that StopTask wraps
// into the returned error.
//
// Returns a timeout error after controlRequestTimeout (or ctx.Done) if the
// CLI never answers.
func (s *Session) StopTask(ctx context.Context, taskID string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("claude: stop_task: empty task_id")
	}
	opName := "stop_task " + taskID
	res, err := s.sendControlRequest(ctx, opName, map[string]any{
		"subtype": "stop_task",
		"task_id": taskID,
	})
	if err != nil {
		return err
	}
	return interpretControlResponse(res, opName)
}

// BackgroundTask moves an in-flight FOREGROUND task (a `local_agent`
// subagent or a foreground Bash) to the background — the control-request
// form of the interactive TUI's Ctrl+B. The `tool_use_id` argument
// targets the ONE task started by that tool_use block; the CLI's schema
// also accepts the parameter omitted (background everything), which AO
// deliberately never sends: the UI's background button lives on a
// specific row.
//
// Verified 2.1.237 (2026-08-22 spike, background_tasks_control_20260822.ndjson):
// the reply is `{subtype:"success", response:{backgrounded:true}}` and
// the CLI then emits `system/task_updated {patch:{is_backgrounded:true}}`,
// a `system/background_tasks_changed` listing the agent, the agent's
// tool_result in the §E5 async-ack shape, and a normal `result` closing
// the freed turn. Completion arrives later as
// `task_updated`/`task_notification` like any async agent.
//
// `response.backgrounded` is checked rather than trusted: the CLI answers
// `subtype:"success"` for a well-formed request even when it matched no
// live foreground task, so success alone would report "backgrounded" for
// a row that kept streaming. A false/absent flag is a descriptive error
// the UI can show.
//
// The ordinary --forward-subagent-text stream stops after the ack. New AO
// sessions also run --session-mirror, which continues those transcript rows
// live; the task_notification output_file remains compatibility recovery for
// a process started before mirror support. See claude-wire.md
// §control_request `background_tasks`.
//
// Returns a timeout error after controlRequestTimeout (or ctx.Done) if
// the CLI never answers.
func (s *Session) BackgroundTask(ctx context.Context, toolUseID string) error {
	toolUseID = strings.TrimSpace(toolUseID)
	if toolUseID == "" {
		return fmt.Errorf("claude: background_tasks: empty tool_use_id")
	}
	opName := "background_tasks " + toolUseID
	res, err := s.sendControlRequest(ctx, opName, map[string]any{
		"subtype":     "background_tasks",
		"tool_use_id": toolUseID,
	})
	if err != nil {
		return err
	}
	if err := interpretControlResponse(res, opName); err != nil {
		return err
	}
	var payload struct {
		Backgrounded *bool `json:"backgrounded"`
	}
	if len(res.payload) > 0 {
		if err := json.Unmarshal(res.payload, &payload); err != nil {
			return fmt.Errorf("claude: %s: unreadable response payload: %w", opName, err)
		}
	}
	if payload.Backgrounded == nil {
		return fmt.Errorf("claude: %s: provider did not report a backgrounded result", opName)
	}
	if !*payload.Backgrounded {
		return fmt.Errorf("claude: %s: provider refused to background the task (no matching foreground task)", opName)
	}
	return nil
}

// replayExpectation is the transcript parent recorded at send time for one
// outbound user message, matched against the replay echo carrying the same
// client-minted uuid.
type replayExpectation struct {
	parent   string
	wasRisky bool
}

// maxExpectedReplayEntries bounds the expectation map. Entries are consumed
// by their echo, but a queued message the CLI cancels never echoes; the cap
// evicts oldest-first so those cannot accumulate. 64 is far above any real
// number of unechoed in-flight sends.
const maxExpectedReplayEntries = 64

// recordExpectedReplayParent stores the current canonical leaf as the
// expected parent for the echo of the message sent under uuid. A uuid-less
// send records nothing — with no key there is no way to attribute its echo,
// and every AO sender stamps a uuid.
func (s *Session) recordExpectedReplayParent(uuid string) {
	if s == nil || uuid == "" {
		return
	}
	var expectation replayExpectation
	if s.leafTracker != nil {
		expectation.parent = s.leafTracker.canonicalLeaf()
		expectation.wasRisky = s.leafTracker.requiresResumeAtBeforeUserSend()
	}
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	if s.expectedReplayByUUID == nil {
		s.expectedReplayByUUID = make(map[string]replayExpectation)
	}
	if _, exists := s.expectedReplayByUUID[uuid]; !exists {
		s.expectedReplayOrder = append(s.expectedReplayOrder, uuid)
	}
	s.expectedReplayByUUID[uuid] = expectation
	for len(s.expectedReplayOrder) > maxExpectedReplayEntries {
		oldest := s.expectedReplayOrder[0]
		s.expectedReplayOrder = s.expectedReplayOrder[1:]
		delete(s.expectedReplayByUUID, oldest)
	}
}

// takeExpectedReplayParent consumes the expectation recorded for uuid.
func (s *Session) takeExpectedReplayParent(uuid string) (expectation replayExpectation, ok bool) {
	if s == nil || uuid == "" {
		return replayExpectation{}, false
	}
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	expectation, ok = s.expectedReplayByUUID[uuid]
	if ok {
		delete(s.expectedReplayByUUID, uuid)
		for i, id := range s.expectedReplayOrder {
			if id == uuid {
				s.expectedReplayOrder = append(s.expectedReplayOrder[:i], s.expectedReplayOrder[i+1:]...)
				break
			}
		}
	}
	return expectation, ok
}

// clearExpectedReplayParent drops the expectation for uuid — called when the
// stdin write failed, so no echo will ever arrive.
func (s *Session) clearExpectedReplayParent(uuid string) {
	if s == nil || uuid == "" {
		return
	}
	_, _ = s.takeExpectedReplayParent(uuid)
}

func (s *Session) verifyReplayParent(evt provider.ProviderEvent) {
	providerItemID, parentUUID := replayProviderIDs(evt.Meta)
	expectation, ok := s.takeExpectedReplayParent(providerItemID)
	if !ok || expectation.parent == "" {
		return
	}
	expectedParent, wasRisky := expectation.parent, expectation.wasRisky
	if parentUUID == "" && wasRisky && providerItemID != "" && s.sessionID != "" {
		if parent, found, err := findReplayUserParent(s.sessionID, s.workDir, providerItemID); err != nil {
			s.emitReplayParentError(fmt.Sprintf("Claude replay omitted parentUuid and AO could not verify the transcript parent: %v", err))
			return
		} else if found {
			parentUUID = parent
		}
	}
	if parentUUID == "" && wasRisky {
		s.emitReplayParentError("Claude replay omitted parentUuid and AO could not verify the transcript parent")
		return
	}
	if parentUUID == "" || parentUUID == expectedParent {
		return
	}
	s.emitReplayParentError(fmt.Sprintf("Claude attached the user message to transcript parent %s, expected %s", parentUUID, expectedParent))
}

func replayProviderIDs(meta json.RawMessage) (providerItemID, parentUUID string) {
	if len(meta) == 0 {
		return "", ""
	}
	var fields struct {
		ProviderItemID string `json:"provider_item_id"`
		ParentUUID     string `json:"parent_uuid"`
	}
	if err := json.Unmarshal(meta, &fields); err != nil {
		return "", ""
	}
	return strings.TrimSpace(fields.ProviderItemID), strings.TrimSpace(fields.ParentUUID)
}

func (s *Session) emitReplayParentError(message string) {
	meta, _ := json.Marshal(map[string]any{
		"fatal": true,
		"code":  "claude_context_parent_mismatch",
	})
	s.onEvent(provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  s.threadID,
		Content:   message,
		Meta:      meta,
		Failure:   &provider.FailureMeta{Class: provider.FailureFatal, Boundary: provider.FailureBoundaryEvent},
		Timestamp: time.Now(),
	})
	if s.proc != nil {
		_ = s.proc.Close()
	}
}
