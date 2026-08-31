package claude

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionimport"
)

const (
	maxPendingMirrorFiles      = 16
	maxPendingMirrorEntries    = 128
	maxPendingMirrorFileBytes  = 2 << 20
	maxPendingMirrorTotalBytes = 8 << 20
	maxSeenMirrorSourceUUIDs   = 8192
)

func (s *transcriptMirrorState) clearPending(key string) {
	s.totalPendingBytes -= s.pendingBytes[key]
	if s.totalPendingBytes < 0 {
		s.totalPendingBytes = 0
	}
	delete(s.pendingBytes, key)
	delete(s.pendingFacts, key)
	delete(s.pendingOwner, key)
	delete(s.pending, key)
	delete(s.pendingWarned, key)
}

func (s *transcriptMirrorState) noteMetadataTaskScope(agentID, toolUseID string) error {
	agentID = strings.TrimSpace(agentID)
	toolUseID = strings.TrimSpace(toolUseID)
	if agentID == "" || toolUseID == "" {
		return nil
	}
	binding := s.taskScopes[agentID]
	// task_started is newer lifecycle evidence when an existing agent is
	// resumed under another tool call. Metadata only closes the earlier race.
	if binding.scope != "" {
		if !binding.needsProjection && binding.scope != toolUseID {
			return fmt.Errorf("agent %q changed agent_metadata toolUseId from %q to %q", agentID, binding.scope, toolUseID)
		}
		return nil
	}
	binding.scope = toolUseID
	binding.needsProjection = s.scopeOwners[toolUseID] != ""
	s.taskScopes[agentID] = binding
	return nil
}

func (s *transcriptMirrorState) warnPendingOnce(key, commandUUID, message string) bool {
	if _, warned := s.pendingWarned[key]; warned {
		return false
	}
	s.pendingWarned[key] = struct{}{}
	log.Printf("claude: transcript_mirror %s for %q", message, key)
	if _, warned := s.degradedCommands[commandUUID]; warned {
		return false
	}
	s.degradedCommands[commandUUID] = struct{}{}
	return true
}

func (s *transcriptMirrorState) bufferPending(key, commandUUID string, entries []json.RawMessage) bool {
	if len(entries) == 0 {
		return false
	}
	degraded := false
	if _, exists := s.pending[key]; !exists && len(s.pending) >= maxPendingMirrorFiles {
		return s.warnPendingOnce(key, commandUUID, "pending-file limit reached; dropping unclassified mirror file")
	}
	if owner := s.pendingOwner[key]; owner != "" && owner != commandUUID {
		return s.warnPendingOnce(key, commandUUID, "command owner changed; dropping unclassified entries")
	}
	pending := s.pending[key]
	room := maxPendingMirrorEntries - len(pending)
	if room <= 0 {
		return s.warnPendingOnce(key, commandUUID, "pending-entry limit reached; dropping unclassified entries")
	}
	if len(entries) > room {
		degraded = s.warnPendingOnce(key, commandUUID, "pending-entry limit reached; truncating unclassified entries")
		entries = entries[:room]
	}
	fileBytes := s.pendingBytes[key]
	accepted := make([]json.RawMessage, 0, len(entries))
	for _, entry := range entries {
		size := len(entry)
		if fileBytes+size > maxPendingMirrorFileBytes || s.totalPendingBytes+size > maxPendingMirrorTotalBytes {
			degraded = s.warnPendingOnce(key, commandUUID, "pending-byte limit reached; dropping unclassified entries") || degraded
			break
		}
		accepted = append(accepted, entry)
		fileBytes += size
		s.totalPendingBytes += size
	}
	entries = accepted
	if len(entries) == 0 {
		return degraded
	}
	s.pending[key] = append(pending, entries...)
	s.pendingOwner[key] = commandUUID
	s.pendingBytes[key] = fileBytes
	return degraded
}

func (s *transcriptMirrorState) newProjection(filePath, scope, agentID, commandUUID string) (*mirrorProjection, error) {
	projector, err := sessionimport.NewSidechainProjector(scope)
	if err != nil {
		return nil, err
	}
	key := mirrorProjectionKey(filePath, agentID)
	projection := &mirrorProjection{
		scope:           scope,
		agentID:         agentID,
		commandUUID:     commandUUID,
		projector:       projector,
		seenSourceUUIDs: make(map[string]struct{}),
		seenSourceOrder: make([]string, 0, 64),
	}
	s.projections[key] = projection
	s.scopeOwners[scope] = key
	if agentID != "" {
		binding := s.taskScopes[agentID]
		binding.scope = scope
		binding.projectionKey = key
		binding.needsProjection = true
		s.taskScopes[agentID] = binding
	}
	return projection, nil
}

func mirrorProjectionKey(filePath, agentID string) string {
	if filePath != "" {
		return filePath
	}
	return "agent:" + agentID
}

func (s *transcriptMirrorState) providerEvents(threadID string, result sessionimport.ConvertResult, projectionKey string) []provider.ProviderEvent {
	events := make([]provider.ProviderEvent, 0, len(result.Events))
	for _, imported := range result.Events {
		event := imported.ProviderEvent
		event.ThreadID = threadID
		if event.Kind == provider.EventUserText && strings.TrimSpace(event.ItemID) != "" {
			event.Meta = mergeJSONMetaValue(event.Meta, "provider_item_id", strings.TrimSpace(event.ItemID))
		}
		event.Meta = mergeJSONMetaFlag(event.Meta, provider.MetaTranscriptSnapshotKey, true)
		events = append(events, event)
		if event.Kind == provider.EventToolStart && (isAgentLaunchToolName(event.ItemType) || event.ItemType == "Skill") {
			s.scopeOwners[event.ItemID] = projectionKey
		}
	}
	for _, warning := range result.Warnings {
		log.Printf("claude: transcript_mirror warning %s: %s", warning.Code, warning.Message)
	}
	return events
}

func mergeJSONMetaFlag(meta json.RawMessage, key string, value bool) json.RawMessage {
	return mergeJSONMetaValue(meta, key, value)
}

func mergeJSONMetaValue(meta json.RawMessage, key string, value any) json.RawMessage {
	fields := map[string]any{}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &fields)
	}
	fields[key] = value
	encoded, _ := json.Marshal(fields)
	return encoded
}

func (p *mirrorProjection) unseen(entries []json.RawMessage, facts []mirrorEntryFact) ([]json.RawMessage, []string) {
	out := make([]json.RawMessage, 0, len(entries))
	uuids := make([]string, 0, len(entries))
	for index, entry := range entries {
		uuid := ""
		if index < len(facts) {
			uuid = facts[index].uuid
		}
		if uuid == "" {
			out = append(out, entry)
			uuids = append(uuids, "")
			continue
		}
		if _, seen := p.seenSourceUUIDs[uuid]; seen {
			continue
		}
		out = append(out, entry)
		uuids = append(uuids, uuid)
	}
	return out, uuids
}

func (p *mirrorProjection) remember(uuids []string) {
	for _, uuid := range uuids {
		if uuid == "" {
			continue
		}
		if _, seen := p.seenSourceUUIDs[uuid]; seen {
			continue
		}
		if len(p.seenSourceOrder) >= maxSeenMirrorSourceUUIDs {
			oldest := p.seenSourceOrder[0]
			p.seenSourceOrder = p.seenSourceOrder[1:]
			delete(p.seenSourceUUIDs, oldest)
			if !p.seenEvicted {
				log.Printf("claude: transcript_mirror source-id window exceeded for scope %q; old replay dedupe is no longer guaranteed", p.scope)
				p.seenEvicted = true
			}
		}
		p.seenSourceUUIDs[uuid] = struct{}{}
		p.seenSourceOrder = append(p.seenSourceOrder, uuid)
	}
}

func (s *transcriptMirrorState) observeBindings(rows []sessionimport.Row, currentScope, projectionKey string) {
	for _, decoded := range rows {
		row := decoded.Raw
		result, _ := row["toolUseResult"].(map[string]any)
		agentID, _ := result["agentId"].(string)
		agentID = strings.TrimSpace(agentID)
		if agentID == "" {
			continue
		}
		message, _ := row["message"].(map[string]any)
		blocks, _ := message["content"].([]any)
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			if block["type"] != "tool_result" {
				continue
			}
			toolUseID, _ := block["tool_use_id"].(string)
			toolUseID = strings.TrimSpace(toolUseID)
			if toolUseID == "" {
				continue
			}
			if owner := s.scopeOwners[toolUseID]; owner == projectionKey || toolUseID == currentScope {
				binding := s.taskScopes[agentID]
				binding.scope = toolUseID
				binding.needsProjection = true
				s.taskScopes[agentID] = binding
			}
		}
	}
}
