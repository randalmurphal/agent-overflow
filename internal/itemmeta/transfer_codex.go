package itemmeta

import (
	"encoding/json"
	"fmt"
)

// TransferCodexSessions remaps execution identities in persisted collaboration
// metadata. The provider copy adapter supplies the complete native ID map.
// Historical messages, arguments of unrelated tools, and unknown extensions
// remain untouched; replacing strings recursively would corrupt user content.
func TransferCodexSessions(raw string, ids map[string]string) (string, error) {
	if raw == "" || len(ids) == 0 {
		return raw, nil
	}
	meta, err := decodeRuntimeObject(raw)
	if err != nil {
		return "", err
	}
	remap := func(id string) string {
		if mapped := ids[id]; mapped != "" {
			return mapped
		}
		return id
	}
	field := func(object map[string]json.RawMessage, key string) {
		var id string
		if json.Unmarshal(object[key], &id) == nil && remap(id) != id {
			object[key], _ = json.Marshal(remap(id))
		}
	}
	keys := func(object map[string]json.RawMessage, key string) error {
		value, exists := object[key]
		if !exists || string(value) == "null" {
			return nil
		}
		var original map[string]json.RawMessage
		if err := json.Unmarshal(value, &original); err != nil {
			return fmt.Errorf("transfer: invalid Codex %s: %w", key, err)
		}
		mapped := make(map[string]json.RawMessage, len(original))
		for id, value := range original {
			id = remap(id)
			if _, exists := mapped[id]; exists {
				return fmt.Errorf("transfer: colliding Codex identities in %s", key)
			}
			mapped[id] = value
		}
		object[key], _ = json.Marshal(mapped)
		return nil
	}
	for _, key := range []string{"agent_path", "agentThreadId", "senderThreadId", "receiverThreadId"} {
		field(meta, key)
	}
	for _, key := range []string{"codex_child_terminal_statuses", "codex_child_resume_generations"} {
		if err := keys(meta, key); err != nil {
			return "", err
		}
	}
	var input map[string]json.RawMessage
	if value := meta["input"]; len(value) != 0 && string(value) != "null" {
		if err := json.Unmarshal(value, &input); err != nil {
			// Non-object inputs of unrelated tools are valid historical data.
			return encodeObject(meta)
		}
	}
	var tool, activity string
	_ = json.Unmarshal(input["tool"], &tool)
	_ = json.Unmarshal(input["activityKind"], &activity)
	if !isTransferCollabTool(tool) && activity == "" {
		return encodeObject(meta)
	}
	for _, key := range []string{"agentThreadId", "senderThreadId", "receiverThreadId", "newThreadId"} {
		field(input, key)
	}
	if value, exists := input["receiverThreadIds"]; exists && string(value) != "null" {
		var receivers []string
		if err := json.Unmarshal(value, &receivers); err != nil {
			return "", err
		}
		for i, id := range receivers {
			receivers[i] = remap(id)
		}
		input["receiverThreadIds"], _ = json.Marshal(receivers)
	}
	if value, exists := input["receiverAgents"]; exists && string(value) != "null" {
		var agents []map[string]json.RawMessage
		if err := json.Unmarshal(value, &agents); err != nil {
			return "", err
		}
		for _, agent := range agents {
			field(agent, "threadId")
		}
		input["receiverAgents"], _ = json.Marshal(agents)
	}
	if err := keys(input, "agentsStates"); err != nil {
		return "", err
	}
	meta["input"], _ = json.Marshal(input)
	return encodeObject(meta)
}

func isTransferCollabTool(name string) bool {
	switch name {
	case "spawn_agent", "send_input", "wait", "wait_agent", "close_agent", "resume_agent", "send_message":
		return true
	}
	return false
}
