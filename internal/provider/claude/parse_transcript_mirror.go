package claude

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionimport"
)

// transcript_mirror is the live, append-only view enabled by
// --session-mirror. It is the only live wire surface for a direct command's
// fork root (2.1.300 replays the finished fork on stdout too;
// mirrorProjectsStdoutRow drops the replay) and for the tail of a foreground agent after background_tasks stops
// forwarding that sidechain on ordinary stdout.
type transcriptMirrorEnvelope struct {
	FilePath       string            `json:"filePath"`
	LegacyFilePath string            `json:"file_path"`
	Entries        []json.RawMessage `json:"entries"`
}

type transcriptMirrorState struct {
	projections       map[string]*mirrorProjection
	taskScopes        map[string]mirrorTaskScope
	scopeOwners       map[string]string
	commands          map[string]*mirroredCommand
	compactionTaps    map[string]*mirrorCompactionTap
	pending           map[string][]json.RawMessage
	pendingFacts      map[string]mirrorEntryInspection
	pendingOwner      map[string]string
	pendingBytes      map[string]int
	totalPendingBytes int
	pendingWarned     map[string]struct{}
	degradedCommands  map[string]struct{}
	// terminalTasks queues, oldest first, the tasks whose projection stays
	// open past their terminal (finishMirroredTask).
	terminalTasks []string
}

type mirrorTaskScope struct {
	scope           string
	projectionKey   string
	terminal        bool
	needsProjection bool
}

type mirrorProjection struct {
	scope             string
	agentID           string
	commandUUID       string
	metadataSeen      bool
	metadataToolUseID string
	projector         *sessionimport.SidechainProjector
	seenSourceUUIDs   map[string]struct{}
	seenSourceOrder   []string
	seenEvicted       bool
}

func (p *mirrorProjection) observeMetadata(facts mirrorEntryInspection) error {
	if !facts.metadataSeen {
		return nil
	}
	if p.metadataSeen && p.metadataToolUseID != facts.metadataToolUseID {
		return fmt.Errorf("projection changed agent_metadata toolUseId from %q to %q", p.metadataToolUseID, facts.metadataToolUseID)
	}
	p.metadataSeen = true
	p.metadataToolUseID = facts.metadataToolUseID
	return nil
}

func (p *Parser) parseTranscriptMirror(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	if p == nil {
		return nil, nil
	}
	var envelope transcriptMirrorEnvelope
	envelope.FilePath = readRawString(raw["filePath"])
	envelope.LegacyFilePath = readRawString(raw["file_path"])
	if err := json.Unmarshal(raw["entries"], &envelope.Entries); err != nil {
		return nil, fmt.Errorf("parse transcript_mirror entries: %w", err)
	}
	// Queue operations are session records, not transcript rows; only a
	// dropped agent wake among them is an event (parse_queue_operation.go).
	entries, dropped := takeQueueOperations(threadID, envelope.Entries, now)
	if len(entries) == 0 {
		return dropped, nil
	}
	envelope.Entries = entries
	events, err := p.parseTranscriptMirrorEntries(threadID, envelope, now)
	return append(dropped, events...), err
}

// parseTranscriptMirrorEntries routes a mirror batch's transcript rows.
func (p *Parser) parseTranscriptMirrorEntries(threadID string, envelope transcriptMirrorEnvelope, now time.Time) ([]provider.ProviderEvent, error) {
	if envelope.FilePath == "" {
		envelope.FilePath = envelope.LegacyFilePath
	}

	state := p.ensureTranscriptMirrorState()
	facts, err := inspectMirrorEntries(envelope.Entries)
	if err != nil {
		return nil, fmt.Errorf("inspect transcript_mirror %q: %w", envelope.FilePath, err)
	}
	if facts.agentID == "" {
		facts.agentID = mirrorAgentIDFromPath(envelope.FilePath)
	}
	projectionKey := mirrorProjectionKey(envelope.FilePath, facts.agentID)
	projection := state.projections[projectionKey]
	var events []provider.ProviderEvent
	entries := envelope.Entries
	// attributionSkill names the skill responsible for work in both the main
	// transcript and a forked skill's sidechain. isSidechain identifies the
	// transcript scope; agent_metadata identifies its owner. Main-transcript
	// rows already arrive on stdout and must never be projected beneath the
	// provisional command row. The main-transcript facts stdout can lack are
	// the record of this app's own prompt and the turns the CLI starts
	// itself; see mainTranscriptEvents.
	if facts.scope == mirrorTranscriptMain {
		state.clearPending(projectionKey)
		if projection != nil {
			return nil, fmt.Errorf("transcript_mirror %q changed from sidechain to main transcript", envelope.FilePath)
		}
		return p.mainTranscriptEvents(threadID, envelope.Entries, facts.entries, now), nil
	}
	if projection == nil && p.activeCommandUUID != "" && state.commands[p.activeCommandUUID] != nil {
		// Claude emits agent_metadata before the ordinary task_started event.
		// Merge only the small classification state on each append. The bounded
		// raw prefix is decoded once, when ownership becomes known.
		classification := state.pendingFacts[projectionKey]
		if err := classification.merge(facts); err != nil {
			return events, fmt.Errorf("inspect buffered transcript_mirror %q: %w", envelope.FilePath, err)
		}
		if err := state.noteMetadataTaskScope(classification.agentID, classification.metadataToolUseID); err != nil {
			return events, fmt.Errorf("inspect transcript_mirror %q: %w", envelope.FilePath, err)
		}
		binding := state.taskScopes[classification.agentID]
		if binding.scope != "" && !binding.needsProjection && state.scopeOwners[binding.scope] != "" {
			// The launch the agent's metadata named was projected after
			// the metadata arrived: the agent runs inside a projected scope.
			binding.needsProjection = true
			state.taskScopes[classification.agentID] = binding
		}
		// A launch inside a projected scope announces its agent before the
		// mirror delivers the launch itself (forked_skill_direct_command),
		// so the agent's batches wait for it like unclassified ones.
		awaitingLaunch := binding.scope != "" && !binding.needsProjection && p.awaitsTaskCall(binding.scope)
		if !classification.provesSkillFork() && (binding.scope == "" || awaitingLaunch) {
			if state.bufferPending(projectionKey, p.activeCommandUUID, envelope.Entries) {
				events = append(events, transcriptMirrorDegradedEvent(
					threadID, p.activeCommandUUID, state.commands[p.activeCommandUUID].launchID, now,
				))
			}
			if len(state.pending[projectionKey]) > 0 {
				state.pendingFacts[projectionKey] = classification
			}
			return events, nil
		}
		if !classification.provesSkillFork() && !binding.needsProjection {
			// The batches are dropped for good here, so compaction rows in
			// them get their one live chance now. Buffered batches never went
			// through the tap (they could still have fed a projection).
			if pending := state.pending[projectionKey]; len(pending) > 0 {
				if pendingFacts, err := inspectMirrorEntries(pending); err == nil {
					if pendingFacts.agentID == "" {
						pendingFacts.agentID = classification.agentID
					}
					events = append(events, p.tapUnprojectedCompaction(threadID, state, pendingFacts, pending, now)...)
				}
			}
			state.clearPending(projectionKey)
			return append(events, p.tapUnprojectedCompaction(threadID, state, facts, entries, now)...), nil
		}
		if pending := state.pending[projectionKey]; len(pending) > 0 {
			combined := make([]json.RawMessage, 0, len(pending)+len(entries))
			combined = append(combined, pending...)
			entries = append(combined, entries...)
			facts, err = inspectMirrorEntries(entries)
			if err != nil {
				return events, fmt.Errorf("inspect buffered transcript_mirror %q: %w", envelope.FilePath, err)
			}
			if facts.agentID == "" {
				facts.agentID = mirrorAgentIDFromPath(envelope.FilePath)
			}
			state.clearPending(projectionKey)
		}
	}

	if projection == nil && facts.agentID != "" {
		if binding := state.taskScopes[facts.agentID]; binding.scope != "" && binding.needsProjection {
			commandUUID := ""
			if owner := state.projections[state.scopeOwners[binding.scope]]; owner != nil {
				commandUUID = owner.commandUUID
			}
			projection, err = state.newProjection(envelope.FilePath, binding.scope, facts.agentID, commandUUID)
			if err != nil {
				return nil, err
			}
			marker, _ := json.Marshal(map[string]any{
				"meta_update_only":                 true,
				provider.MetaTranscriptMirroredKey: true,
			})
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventToolStart,
				ThreadID:  threadID,
				ItemID:    binding.scope,
				Meta:      marker,
				Timestamp: facts.timestampOr(now),
			})
		}
	}

	// A direct command fork is identified from the wire, not from a list of
	// commands AO believes may fork. The active stdin command proves the
	// outer command. A sidechain agent_metadata row without toolUseId proves
	// Claude created a Skill fork. Ordinary Agent sidechains carry toolUseId
	// and can inherit the surrounding inline skill's attribution.
	// A bound agent is not this command's fork: the fork's own task_started
	// names no tool_use, while a resumed fork and a forked Skill call
	// inside the fork are bound by theirs.
	if projection == nil && p.activeCommandUUID != "" && facts.provesSkillFork() && p.taskToolUseRef(facts.agentID).ToolUseID == "" {
		commandState := state.commands[p.activeCommandUUID]
		if commandState != nil {
			launchID := commandState.launchID
			if launchID == "" {
				launchID = directCommandLaunchID(p.activeCommandUUID)
			}
			commandState.launchID = launchID
			commandState.agentID = facts.agentID
			commandState.rootProjectionKey = projectionKey
			commandState.forked = true
			projection, err = state.newProjection(envelope.FilePath, launchID, facts.agentID, p.activeCommandUUID)
			if err != nil {
				return nil, err
			}
			skillName := strings.TrimSpace(facts.attributionSkill)
			if skillName == "" {
				skillName = commandState.command.Name
			}
			commandState.skillName = skillName
			input := map[string]any{"skill": skillName}
			if commandState.command.Argument != "" {
				input["args"] = commandState.command.Argument
			}
			// The fork's agentId is its task id. Its task_started named no
			// tool_use, so this is where the fork's terminal finds its row,
			// and where a later resume finds the root its rows return to.
			p.rememberTaskToolUse(facts.agentID, launchID)
			p.rememberTaskTranscriptRoot(facts.agentID, launchID)
			metaFields := map[string]any{
				"toolName":          "Skill",
				"input":             input,
				"directCommandFork": true,
				"skillFork": map[string]string{
					"agentId":     facts.agentID,
					"commandName": skillName,
				},
				provider.MetaTranscriptMirroredKey: true,
			}
			// The task the row can be stopped by, when the CLI announced one.
			if commandState.forkTaskID == facts.agentID {
				metaFields["task_id"] = facts.agentID
				metaFields["task_type"] = "local_agent"
			}
			if commandState.provisional {
				metaFields["meta_update_only"] = true
			}
			meta, _ := json.Marshal(metaFields)
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventToolStart,
				ThreadID:  threadID,
				ItemID:    launchID,
				ItemType:  "Skill",
				Meta:      meta,
				Timestamp: facts.timestampOr(now),
			})
		}
	}

	if projection == nil {
		// Ordinary async agents already stream on stdout. Mirroring those too
		// would duplicate their live deltas. A projection is opened only for
		// the two gaps above, or for a child launch owned by one of them.
		// Compaction rows are the one exception the stdout feed never
		// carries, so they are tapped out of the dropped batch.
		return append(events, p.tapUnprojectedCompaction(threadID, state, facts, entries, now)...), nil
	}
	if err := projection.observeMetadata(facts); err != nil {
		return events, fmt.Errorf("inspect transcript_mirror %q: %w", envelope.FilePath, err)
	}

	if pending := state.pending[projectionKey]; len(pending) > 0 {
		entries = pending
		state.clearPending(projectionKey)
	}
	entries, unseenFacts := projection.unseen(entries, facts.entries)
	if len(entries) == 0 {
		return events, nil
	}
	rows, err := sessionimport.DecodeSidechainRows(entries, now)
	if err != nil {
		return events, fmt.Errorf("parse transcript_mirror %q: %w", envelope.FilePath, err)
	}
	result, err := projection.projector.AppendRows(rows)
	if err != nil {
		return events, fmt.Errorf("parse transcript_mirror %q: %w", envelope.FilePath, err)
	}
	projection.remember(unseenFacts)
	projected := state.providerEvents(threadID, result, projectionKey)
	events = append(events, p.observeProjectedToolCalls(threadID, projected)...)
	state.observeBindings(rows, projection.scope, projectionKey)
	return events, nil
}

// mainTranscriptEvents reads the main-transcript user rows of one mirror
// batch, in order, for the facts stdout can lack: the record of this app's
// own prompt (recordedOwnPrompt) and a turn the CLI started itself
// (appendTurnStart). Every other main row already arrived on stdout. A row
// under a uuid this app issued is its own send whatever else the row says.
func (p *Parser) mainTranscriptEvents(threadID string, entries []json.RawMessage, facts []mirrorEntryFact, now time.Time) []provider.ProviderEvent {
	var events []provider.ProviderEvent
	for i, fact := range facts {
		if i >= len(entries) || !fact.user || fact.uuid == "" {
			continue
		}
		if p.peerTurns != nil && p.peerTurns.commandUUIDIsIssued(fact.uuid) {
			if event, ok := recordedOwnPrompt(threadID, entries[i], fact.uuid, now); ok {
				events = append(events, event)
			}
			continue
		}
		events = p.appendTurnStart(events, threadID, entries[i], fact, now)
	}
	return events
}

// recordedOwnPrompt reports a main-transcript user entry written under a
// uuid this app issued, as a confirm-only EventUserText.
//
// The stdout echo is not a reliable confirmation for a prompt that opens a
// turn: the CLI echoes it only when its replay filter accepts the text, and
// that filter rejects any prompt containing a tag such as
// `<task-notification>` or `<bash-stdout>` (verified 2.1.284). The
// transcript write is unconditional and lands before the turn's first
// output, so it confirms the send whatever the message says. A prompt
// drained into a running turn is recorded as a `queued_command` attachment,
// not a user entry, and keeps its echo; it is confirmed there.
//
// The ledger gate keeps every other user entry, tool results and CLI
// injections alike, off this path. With no session ledger nothing is
// provably ours, so nothing is reported.
func recordedOwnPrompt(threadID string, raw json.RawMessage, uuid string, now time.Time) (provider.ProviderEvent, bool) {
	var entry struct {
		ParentUUID string `json:"parentUuid"`
		Message    struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		log.Printf("claude: transcript_mirror prompt %s unreadable: %v", uuid, err)
		return provider.ProviderEvent{}, false
	}
	fields := map[string]any{
		"provider_item_id":                         uuid,
		provider.MetaUserTextTranscriptRecordedKey: true,
	}
	if entry.ParentUUID != "" {
		fields["parent_uuid"] = entry.ParentUUID
	}
	// The same fingerprint the echo carries, so a queue-boundary merge
	// is recognised from whichever confirmation arrives first.
	if digest := EchoBlockDigest(entry.Message.Content); len(digest) > 0 {
		fields[provider.MetaUserContentBlockDigestKey] = []string(digest)
	}
	meta, err := json.Marshal(fields)
	if err != nil {
		log.Printf("claude: encode transcript_mirror prompt %s: %v", uuid, err)
		return provider.ProviderEvent{}, false
	}
	return provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  threadID,
		Content:   extractToolResultText(entry.Message.Content),
		Meta:      meta,
		Timestamp: now,
	}, true
}

// tapUnprojectedCompaction forwards the compaction rows of a mirror batch
// that is otherwise dropped because its agent already streams on stdout.
// The stdout feed never carries a sidechain's `system/compact_boundary` or
// `isCompactSummary` rows (claude-wire.md §Subagent stream forwarding), so
// without this the boundary never reaches the thread. While a boundary
// waits for its summary, the tap also forwards attachment rows: the CLI
// can chain the summary below the boundary through them, and the
// projector pairs the two through that chain. Other attachments are left
// undecoded.
//
// Failure here is logged and the boundary is dropped rather than failing
// the parse: completion never reads the transcript, so nothing delivers
// it later.
func (p *Parser) tapUnprojectedCompaction(
	threadID string,
	state *transcriptMirrorState,
	facts mirrorEntryInspection,
	entries []json.RawMessage,
	now time.Time,
) []provider.ProviderEvent {
	agentID := facts.agentID
	if agentID == "" {
		return nil
	}
	existing := state.compactionTaps[agentID]
	awaitingSummary := existing != nil && existing.projector.AwaitingCompactSummary()
	picked := make([]json.RawMessage, 0, 2)
	uuids := make([]string, 0, 2)
	for i, entry := range entries {
		if i >= len(facts.entries) {
			break
		}
		fact := facts.entries[i]
		switch {
		case fact.compaction && fact.uuid != "":
			if existing != nil {
				if _, seen := existing.seen[fact.uuid]; seen {
					continue
				}
			}
			picked = append(picked, entry)
			uuids = append(uuids, fact.uuid)
			awaitingSummary = fact.compactBoundary
		case fact.attachment && awaitingSummary:
			picked = append(picked, entry)
		}
	}
	if len(picked) == 0 {
		return nil
	}

	// The launch tool call is the scope every row lands under, and for a
	// resumed agent that is the TRANSCRIPT ROOT — the original launch —
	// never the newest carrier: the lifecycle rebinds onto the carrier
	// but the agent's conversation tree does not move (claude-wire.md
	// §E6). The current binding is the fallback when no root is known
	// (an earlier process launched the agent and the history named
	// none); triage's carrier-parent rewrite covers the rest.
	scope := p.taskTranscriptRoot(agentID)
	if scope == "" {
		scope = state.taskScopes[agentID].scope
	}
	if scope == "" {
		scope = p.taskToolUseRef(agentID).ToolUseID
	}
	if scope == "" {
		log.Printf("claude: transcript_mirror compaction for agent %q has no resolvable launch tool_use; dropping it", agentID)
		return nil
	}
	tap, retargeted, err := state.compactionTap(threadID, agentID, scope)
	if err != nil {
		log.Printf("claude: transcript_mirror compaction tap for agent %q: %v; dropping it", agentID, err)
		return retargeted
	}
	rows, err := sessionimport.DecodeSidechainRows(picked, now)
	if err != nil {
		log.Printf("claude: transcript_mirror compaction tap decode for agent %q: %v; dropping it", agentID, err)
		return retargeted
	}
	for _, uuid := range uuids {
		tap.seen[uuid] = struct{}{}
	}
	result, err := tap.projector.AppendRows(rows)
	if err != nil {
		log.Printf("claude: transcript_mirror compaction tap for agent %q: %v; dropping it", agentID, err)
		return retargeted
	}
	return append(retargeted, tapCompactionProviderEvents(threadID, agentID, result)...)
}

func (p *Parser) ensureTranscriptMirrorState() *transcriptMirrorState {
	if p.transcriptMirror == nil {
		p.transcriptMirror = &transcriptMirrorState{
			projections:      make(map[string]*mirrorProjection),
			taskScopes:       make(map[string]mirrorTaskScope),
			scopeOwners:      make(map[string]string),
			commands:         make(map[string]*mirroredCommand),
			compactionTaps:   make(map[string]*mirrorCompactionTap),
			pending:          make(map[string][]json.RawMessage),
			pendingFacts:     make(map[string]mirrorEntryInspection),
			pendingOwner:     make(map[string]string),
			pendingBytes:     make(map[string]int),
			pendingWarned:    make(map[string]struct{}),
			degradedCommands: make(map[string]struct{}),
		}
	}
	return p.transcriptMirror
}

func transcriptMirrorDegradedEvent(threadID, commandUUID, launchID string, now time.Time) provider.ProviderEvent {
	if launchID == "" {
		launchID = directCommandLaunchID(commandUUID)
	}
	meta, _ := json.Marshal(map[string]string{"kind": "transcript_mirror_degraded"})
	return provider.ProviderEvent{
		Kind:            provider.EventNotification,
		ThreadID:        threadID,
		ItemID:          "transcript-mirror-degraded:" + commandUUID,
		Content:         "Some early agent activity could not be shown because Claude's transcript mirror exceeded Agent Overflow's safety bound.",
		ContentPresent:  true,
		Meta:            meta,
		ParentToolUseID: launchID,
		Timestamp:       now,
	}
}

// finishMirroredTask runs at a task's stop (its task_notification) and at
// its kill (final). Claude writes an agent's last message to its
// transcript after the task's terminal (every typed fork and projected
// agent capture, claude-wire.md §E9), so the terminal does not close the
// agent's projection: it stays open for that message until its command
// ends, the agent is resumed, the session closes, or
// maxTerminalMirrorTasks later terminals retire it.
//
// An agent that stops while a background task it owns is still running is
// PARKED, not done: the CLI wakes it when the task reports
// (claude-wire.md §E6b), and its woken rounds arrive only through its
// projection, so its binding is not marked terminal at all. A killed agent
// never wakes and is marked regardless.
func (p *Parser) finishMirroredTask(threadID, taskID string, final bool) []provider.ProviderEvent {
	if p == nil || p.transcriptMirror == nil {
		return nil
	}
	state := p.transcriptMirror
	// The task terminal is the last mirror signal this agent gets; a tapped
	// boundary still waiting for its summary row flushes here, BEFORE the
	// notification event the caller appends, so triage persists it before
	// the completion settles the agent.
	events := state.drainCompactionTap(threadID, taskID)
	binding := state.taskScopes[taskID]
	if binding.scope == "" {
		return events
	}
	if !binding.needsProjection {
		delete(state.taskScopes, taskID)
		return events
	}
	if !final && p.ownsLiveBackgroundTask(binding.scope) {
		return events
	}
	binding.terminal = true
	state.taskScopes[taskID] = binding
	return append(events, state.retireTerminalTask(threadID, taskID)...)
}

// maxTerminalMirrorTasks bounds the projections kept open past their
// agent's terminal for its last message. The message lands within the
// same flush as the terminal; this many later terminals is far beyond it.
const maxTerminalMirrorTasks = 64

// retireTerminalTask queues a terminal task's projection and closes the
// oldest one past maxTerminalMirrorTasks. A queued task whose binding was
// released, or resumed, since is skipped.
func (s *transcriptMirrorState) retireTerminalTask(threadID, taskID string) []provider.ProviderEvent {
	s.terminalTasks = slices.DeleteFunc(s.terminalTasks, func(queued string) bool { return queued == taskID })
	s.terminalTasks = append(s.terminalTasks, taskID)
	var events []provider.ProviderEvent
	for len(s.terminalTasks) > maxTerminalMirrorTasks {
		oldest := s.terminalTasks[0]
		s.terminalTasks = s.terminalTasks[1:]
		if s.taskScopes[oldest].terminal {
			events = append(events, s.closeTaskProjection(threadID, oldest)...)
		}
	}
	return events
}

// closeTaskProjection closes the agent's projection, flushing what its
// projector holds, and releases its binding.
func (s *transcriptMirrorState) closeTaskProjection(threadID, taskID string) []provider.ProviderEvent {
	binding := s.taskScopes[taskID]
	delete(s.taskScopes, taskID)
	projection := s.projections[binding.projectionKey]
	if projection == nil || projection.agentID != taskID {
		return nil
	}
	events := s.providerEvents(threadID, projection.projector.Close(), binding.projectionKey)
	s.removeProjection(binding.projectionKey)
	return events
}

// resumeMirrorTask points a resumed agent's mirror binding at its
// transcript root. A typed fork's root is its command row: it has no
// launching tool_use for Claude to forward its rows under, so the mirror
// stays the only surface they reach and they are projected under the root
// again (local_agent_resumed_by_typed_fork_20261014). An agent with a
// launching tool_use streams its resumed rounds on stdout (captured for an
// awaited agent), so a projection its earlier run left open only for its
// last message is closed rather than fed twice. A parked agent's
// projection is left as it is: no capture shows where a parked agent's
// resumed rounds go.
func (p *Parser) resumeMirrorTask(threadID, taskID, root string) []provider.ProviderEvent {
	if p == nil {
		return nil
	}
	if !isDirectCommandLaunchID(root) {
		if p.transcriptMirror == nil || !p.transcriptMirror.taskScopes[taskID].terminal {
			return nil
		}
		return p.transcriptMirror.closeTaskProjection(threadID, taskID)
	}
	state := p.ensureTranscriptMirrorState()
	var events []provider.ProviderEvent
	binding := state.taskScopes[taskID]
	if binding.scope != root {
		events = state.closeTaskProjection(threadID, taskID)
		binding = mirrorTaskScope{scope: root}
	}
	binding.needsProjection = true
	binding.terminal = false
	state.taskScopes[taskID] = binding
	return events
}

// awaitsTaskCall reports whether a task is bound to toolUseID, a call the
// session mirror has not delivered yet (unclassifiedTaskCalls).
func (p *Parser) awaitsTaskCall(toolUseID string) bool {
	if p == nil {
		return false
	}
	_, ok := p.unclassifiedTaskCalls.get(toolUseID)
	return ok
}

// observeProjectedToolCalls records each tool call a projection emitted
// the way the stdout path records a forwarded one, so a background task a
// projected call starts resolves to the agent that owns it
// (ownsLiveBackgroundTask) and its task_started is classified by the call
// that started it. It also finishes what the stdout path does at a call's
// task_started and result:
//
//   - A call a task_started bound before the mirror delivered it is
//     classified now. A launch records the task's transcript root; any
//     other call is a §E6 resume, and its resume events follow the call.
//   - A resume carrier's completion is the resume's ack and carries
//     is_background, as parse_user.go marks one on stdout.
func (p *Parser) observeProjectedToolCalls(threadID string, events []provider.ProviderEvent) []provider.ProviderEvent {
	out := make([]provider.ProviderEvent, 0, len(events))
	for _, event := range events {
		switch event.Kind {
		case provider.EventToolStart:
			if event.ItemID == "" {
				break
			}
			p.stampProjectedAgentLaunchEffort(&event)
			p.rememberToolUseName(event.ItemID, event.ItemType)
			if event.ParentToolUseID != "" {
				p.rememberToolUseParent(event.ItemID, event.ParentToolUseID)
			}
			start, unclassified := p.unclassifiedTaskCalls.get(event.ItemID)
			if !unclassified || event.ItemType == "" {
				break
			}
			p.unclassifiedTaskCalls.delete(event.ItemID)
			if start.resumes == "" && startsAgent(event.ItemType) {
				p.rememberTaskTranscriptRoot(start.taskID, event.ItemID)
				break
			}
			fields, prompt, closed := p.beginTaskResume(threadID, event.ItemID, start.resumes, start, event.Timestamp)
			fields["task_id"] = start.taskID
			fields["task_type"] = "local_agent"
			meta, _ := json.Marshal(fields)
			out = append(out, event)
			out = append(out, closed...)
			out = append(out, provider.ProviderEvent{
				Kind:            provider.EventToolStart,
				ThreadID:        threadID,
				ItemID:          event.ItemID,
				Meta:            meta,
				ParentToolUseID: event.ParentToolUseID,
				Timestamp:       event.Timestamp,
			})
			if prompt != nil {
				out = append(out, *prompt)
			}
			continue
		case provider.EventToolComplete:
			if p.backgroundOrigin(event.ItemID) == backgroundFromTaskStarted {
				event.Meta = mergeJSONMetaFlag(event.Meta, "is_background", true)
				p.clearBackground(event.ItemID)
			}
		}
		out = append(out, event)
	}
	return out
}

func (s *transcriptMirrorState) removeProjection(key string) {
	projection := s.projections[key]
	if projection == nil {
		return
	}
	delete(s.projections, key)
	for taskID, binding := range s.taskScopes {
		if binding.projectionKey == key || s.scopeOwners[binding.scope] == key || taskID == projection.agentID {
			delete(s.taskScopes, taskID)
		}
	}
	for scope, owner := range s.scopeOwners {
		if owner == key {
			delete(s.scopeOwners, scope)
		}
	}
}

func (p *Parser) noteMirrorTaskScope(taskID, toolUseID string, requireOwned bool) {
	if p == nil || strings.TrimSpace(taskID) == "" || strings.TrimSpace(toolUseID) == "" {
		return
	}
	state := p.ensureTranscriptMirrorState()
	if requireOwned {
		if state.scopeOwners[toolUseID] == "" {
			return
		}
	}
	binding := state.taskScopes[taskID]
	if binding.scope != "" && binding.scope != toolUseID {
		// A resumed agent is rebound to the new carrier. Its previous
		// projection belongs to the earlier launch and must not migrate.
		binding = mirrorTaskScope{}
	}
	binding.scope = toolUseID
	binding.needsProjection = true
	state.taskScopes[taskID] = binding
}

// mirrorProjectsStdoutRow reports whether a stdout assistant or user row
// belongs to an agent whose rows the session mirror projects under a direct
// command: a typed fork, or an agent the fork launched. Claude 2.1.300 also
// replays such a fork's turns on stdout after its task_notification, each
// row naming its agent in agent_id; the fork's own rows carry
// parent_tool_use_id "forked-command-<skill>", which no item has. The
// projection already delivers those rows, so the replay is dropped. A fork
// the mirror did not project keeps its stdout rows, and an older CLI sends
// no agent_id.
func (p *Parser) mirrorProjectsStdoutRow(raw map[string]json.RawMessage) bool {
	if p == nil || p.transcriptMirror == nil {
		return false
	}
	agentID := readRawString(raw["agent_id"])
	if agentID == "" || readRawString(raw["parent_tool_use_id"]) == "" {
		return false
	}
	state := p.transcriptMirror
	binding := state.taskScopes[agentID]
	if !binding.needsProjection {
		return false
	}
	projection := state.projections[binding.projectionKey]
	return projection != nil && projection.agentID == agentID && projection.commandUUID != ""
}

func (p *Parser) closeTranscriptMirrors() {
	if p == nil || p.transcriptMirror == nil {
		return
	}
	for _, projection := range p.transcriptMirror.projections {
		projection.projector.Close()
	}
	for _, tap := range p.transcriptMirror.compactionTaps {
		tap.projector.Close()
	}
	p.transcriptMirror = nil
}

type mirrorEntryInspection struct {
	agentID           string
	attributionSkill  string
	scope             mirrorTranscriptScope
	metadataSeen      bool
	metadataToolUseID string
	firstTimestamp    time.Time
	entries           []mirrorEntryFact
}

type mirrorTranscriptScope uint8

const (
	mirrorTranscriptUnknown mirrorTranscriptScope = iota
	mirrorTranscriptMain
	mirrorTranscriptSidechain
)

type mirrorEntryFact struct {
	uuid string
	// compaction marks a `system/compact_boundary` row (compactBoundary)
	// or its `isCompactSummary` row: the two row shapes the stdout
	// forwarding path never carries. The compaction tap feeds these, plus
	// attachment rows while a boundary waits for its summary.
	compaction      bool
	compactBoundary bool
	attachment      bool
	// user marks a `type:"user"` row; turnOrigin is its `turnOrigin`: why
	// the CLI started the turn this row opens (parse_turn_origin.go).
	user       bool
	turnOrigin string
}

func (i mirrorEntryInspection) timestampOr(fallback time.Time) time.Time {
	if !i.firstTimestamp.IsZero() {
		return i.firstTimestamp
	}
	return fallback
}

func (i mirrorEntryInspection) provesSkillFork() bool {
	return i.scope == mirrorTranscriptSidechain && i.attributionSkill != "" && i.metadataSeen && i.metadataToolUseID == ""
}

func (i *mirrorEntryInspection) merge(next mirrorEntryInspection) error {
	if i.scope != mirrorTranscriptUnknown && next.scope != mirrorTranscriptUnknown && i.scope != next.scope {
		return fmt.Errorf("batch mixes main-transcript and sidechain rows")
	}
	if i.scope == mirrorTranscriptUnknown {
		i.scope = next.scope
	}
	if next.metadataSeen {
		if err := i.observeAgentMetadata(next.metadataToolUseID); err != nil {
			return err
		}
	}
	i.agentID = firstNonEmpty(i.agentID, next.agentID)
	i.attributionSkill = firstNonEmpty(i.attributionSkill, next.attributionSkill)
	if !next.firstTimestamp.IsZero() && (i.firstTimestamp.IsZero() || next.firstTimestamp.Before(i.firstTimestamp)) {
		i.firstTimestamp = next.firstTimestamp
	}
	return nil
}

func (i *mirrorEntryInspection) observeAgentMetadata(toolUseID string) error {
	toolUseID = strings.TrimSpace(toolUseID)
	if i.metadataSeen && i.metadataToolUseID != toolUseID {
		return fmt.Errorf("batch has conflicting agent_metadata toolUseId values %q and %q", i.metadataToolUseID, toolUseID)
	}
	i.metadataSeen = true
	i.metadataToolUseID = toolUseID
	return nil
}

func (i *mirrorEntryInspection) observeScope(value *bool) error {
	if value == nil {
		return nil
	}
	next := mirrorTranscriptMain
	if *value {
		next = mirrorTranscriptSidechain
	}
	if i.scope != mirrorTranscriptUnknown && i.scope != next {
		return fmt.Errorf("batch mixes main-transcript and sidechain rows")
	}
	i.scope = next
	return nil
}

func inspectMirrorEntries(entries []json.RawMessage) (mirrorEntryInspection, error) {
	facts := mirrorEntryInspection{entries: make([]mirrorEntryFact, 0, len(entries))}
	for _, entry := range entries {
		var raw struct {
			Type             string `json:"type"`
			Subtype          string `json:"subtype"`
			UUID             string `json:"uuid"`
			AgentID          string `json:"agentId"`
			LegacyAgentID    string `json:"agent_id"`
			ToolUseID        string `json:"toolUseId"`
			LegacyToolUseID  string `json:"tool_use_id"`
			IsSidechain      *bool  `json:"isSidechain"`
			LegacySidechain  *bool  `json:"is_sidechain"`
			IsCompactSummary bool   `json:"isCompactSummary"`
			AttributionSkill string `json:"attributionSkill"`
			LegacySkill      string `json:"attribution_skill"`
			Timestamp        string `json:"timestamp"`
			TurnOrigin       string `json:"turnOrigin"`
			Attribution      struct {
				Skill     string `json:"skill"`
				SkillName string `json:"skillName"`
			} `json:"attribution"`
		}
		if err := json.Unmarshal(entry, &raw); err != nil {
			facts.entries = append(facts.entries, mirrorEntryFact{})
			continue
		}
		if raw.Type == "agent_metadata" {
			if err := facts.observeAgentMetadata(firstNonEmpty(raw.ToolUseID, raw.LegacyToolUseID)); err != nil {
				return mirrorEntryInspection{}, err
			}
		}
		if err := facts.observeScope(raw.IsSidechain); err != nil {
			return mirrorEntryInspection{}, err
		}
		if err := facts.observeScope(raw.LegacySidechain); err != nil {
			return mirrorEntryInspection{}, err
		}
		boundary := raw.Type == "system" && raw.Subtype == "compact_boundary"
		fact := mirrorEntryFact{
			uuid:            strings.TrimSpace(raw.UUID),
			compaction:      boundary || raw.IsCompactSummary,
			compactBoundary: boundary,
			attachment:      raw.Type == "attachment",
			user:            raw.Type == "user",
		}
		if fact.user {
			fact.turnOrigin = strings.TrimSpace(raw.TurnOrigin)
		}
		facts.entries = append(facts.entries, fact)
		facts.agentID = firstNonEmpty(facts.agentID,
			strings.TrimSpace(raw.AgentID), strings.TrimSpace(raw.LegacyAgentID))
		facts.attributionSkill = firstNonEmpty(facts.attributionSkill,
			strings.TrimSpace(raw.AttributionSkill), strings.TrimSpace(raw.LegacySkill),
			strings.TrimSpace(raw.Attribution.Skill), strings.TrimSpace(raw.Attribution.SkillName))
		if facts.firstTimestamp.IsZero() && raw.Timestamp != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, raw.Timestamp); err == nil {
				facts.firstTimestamp = parsed
			}
		}
	}
	return facts, nil
}

func mirrorAgentIDFromPath(path string) string {
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	return strings.TrimPrefix(name, "agent-")
}
