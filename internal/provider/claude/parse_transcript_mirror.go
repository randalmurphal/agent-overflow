package claude

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionimport"
)

// transcript_mirror is the live, append-only view enabled by
// --session-mirror. It is the only wire surface for a direct command's fork
// root and for the tail of a foreground agent after background_tasks stops
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
	pending           map[string][]json.RawMessage
	pendingOwner      map[string]string
	pendingBytes      map[string]int
	totalPendingBytes int
	pendingWarned     map[string]struct{}
	degradedCommands  map[string]struct{}
}

type mirrorTaskScope struct {
	scope         string
	projectionKey string
	terminal      bool
}

type mirrorProjection struct {
	scope           string
	agentID         string
	commandUUID     string
	projector       *sessionimport.SidechainProjector
	seenSourceUUIDs map[string]struct{}
	seenSourceOrder []string
	seenEvicted     bool
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
	if len(envelope.Entries) == 0 {
		return nil, nil
	}
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
	// transcript and a forked skill's sidechain. isSidechain is the ownership
	// fact. Main-transcript rows already arrive on stdout and must never be
	// projected beneath the provisional command row.
	if facts.scope == mirrorTranscriptMain {
		state.clearPending(projectionKey)
		if projection != nil {
			return nil, fmt.Errorf("transcript_mirror %q changed from sidechain to main transcript", envelope.FilePath)
		}
		return nil, nil
	}
	if projection == nil && p.activeCommandUUID != "" && state.commands[p.activeCommandUUID] != nil {
		if !facts.provesSkillFork() {
			if state.bufferPending(projectionKey, p.activeCommandUUID, entries) {
				events = append(events, transcriptMirrorDegradedEvent(
					threadID, p.activeCommandUUID, state.commands[p.activeCommandUUID].launchID, now,
				))
			}
			// A direct-command fork has no launch scope until sidechain attribution
			// proves it is a Skill. Keep its small prefix buffered without rescanning
			// the whole prefix on every append. A manually-backgrounded/nested task
			// already has a scope and can project immediately.
			if state.taskScopes[facts.agentID].scope == "" {
				return events, nil
			}
			entries = state.pending[projectionKey]
			facts, err = inspectMirrorEntries(entries)
			if err != nil {
				return events, fmt.Errorf("inspect buffered transcript_mirror %q: %w", envelope.FilePath, err)
			}
			if facts.agentID == "" {
				facts.agentID = mirrorAgentIDFromPath(envelope.FilePath)
			}
		} else if pending := state.pending[projectionKey]; len(pending) > 0 {
			combined := make([]json.RawMessage, 0, len(pending)+len(entries))
			combined = append(combined, pending...)
			entries = append(combined, entries...)
			facts, err = inspectMirrorEntries(entries)
			if err != nil {
				return events, fmt.Errorf("inspect buffered transcript_mirror %q: %w", envelope.FilePath, err)
			}
			state.clearPending(projectionKey)
		}
	}

	if projection == nil && facts.agentID != "" {
		if binding := state.taskScopes[facts.agentID]; binding.scope != "" {
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
	// outer command, while isSidechain plus attributionSkill prove that Claude
	// created a Skill sidechain for it. Attribution alone also labels ordinary
	// main-agent work performed after an inline skill injected context.
	if projection == nil && p.activeCommandUUID != "" && facts.provesSkillFork() {
		commandState := state.commands[p.activeCommandUUID]
		if commandState != nil {
			launchID := commandState.launchID
			if launchID == "" {
				launchID = "claude-command:" + p.activeCommandUUID
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
		return events, nil
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
	events = append(events, state.providerEvents(threadID, result, projectionKey)...)
	state.observeBindings(rows, projection.scope, projectionKey)
	if binding := state.taskScopes[projection.agentID]; binding.terminal && binding.projectionKey == projectionKey {
		closed := projection.projector.Close()
		events = append(events, state.providerEvents(threadID, closed, projectionKey)...)
		state.removeProjection(projectionKey)
	}
	return events, nil
}

func (p *Parser) ensureTranscriptMirrorState() *transcriptMirrorState {
	if p.transcriptMirror == nil {
		p.transcriptMirror = &transcriptMirrorState{
			projections:      make(map[string]*mirrorProjection),
			taskScopes:       make(map[string]mirrorTaskScope),
			scopeOwners:      make(map[string]string),
			commands:         make(map[string]*mirroredCommand),
			pending:          make(map[string][]json.RawMessage),
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
		launchID = "claude-command:" + commandUUID
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

func (p *Parser) finishMirroredTask(threadID, taskID string) []provider.ProviderEvent {
	if p == nil || p.transcriptMirror == nil {
		return nil
	}
	state := p.transcriptMirror
	binding := state.taskScopes[taskID]
	if binding.scope == "" {
		return nil
	}
	key := binding.projectionKey
	projection := state.projections[key]
	if projection == nil || projection.agentID != taskID {
		binding.terminal = true
		state.taskScopes[taskID] = binding
		return nil
	}
	delete(state.taskScopes, taskID)
	result := projection.projector.Close()
	events := state.providerEvents(threadID, result, key)
	state.removeProjection(key)
	return events
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
	state.taskScopes[taskID] = mirrorTaskScope{scope: toolUseID}
}

func (p *Parser) closeTranscriptMirrors() {
	if p == nil || p.transcriptMirror == nil {
		return
	}
	for _, projection := range p.transcriptMirror.projections {
		projection.projector.Close()
	}
	p.transcriptMirror = nil
}

type mirrorEntryInspection struct {
	agentID          string
	attributionSkill string
	scope            mirrorTranscriptScope
	firstTimestamp   time.Time
	entries          []mirrorEntryFact
}

type mirrorTranscriptScope uint8

const (
	mirrorTranscriptUnknown mirrorTranscriptScope = iota
	mirrorTranscriptMain
	mirrorTranscriptSidechain
)

type mirrorEntryFact struct {
	uuid string
}

func (i mirrorEntryInspection) timestampOr(fallback time.Time) time.Time {
	if !i.firstTimestamp.IsZero() {
		return i.firstTimestamp
	}
	return fallback
}

func (i mirrorEntryInspection) provesSkillFork() bool {
	return i.scope == mirrorTranscriptSidechain && i.attributionSkill != ""
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
			UUID             string `json:"uuid"`
			AgentID          string `json:"agentId"`
			LegacyAgentID    string `json:"agent_id"`
			IsSidechain      *bool  `json:"isSidechain"`
			LegacySidechain  *bool  `json:"is_sidechain"`
			AttributionSkill string `json:"attributionSkill"`
			LegacySkill      string `json:"attribution_skill"`
			Timestamp        string `json:"timestamp"`
			Attribution      struct {
				Skill     string `json:"skill"`
				SkillName string `json:"skillName"`
			} `json:"attribution"`
		}
		if err := json.Unmarshal(entry, &raw); err != nil {
			facts.entries = append(facts.entries, mirrorEntryFact{})
			continue
		}
		if err := facts.observeScope(raw.IsSidechain); err != nil {
			return mirrorEntryInspection{}, err
		}
		if err := facts.observeScope(raw.LegacySidechain); err != nil {
			return mirrorEntryInspection{}, err
		}
		facts.entries = append(facts.entries, mirrorEntryFact{uuid: strings.TrimSpace(raw.UUID)})
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
