package triage

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// Codex reports a spawned child's identity after the spawn activity: the
// nickname and role on the child's thread/started, the effective model and
// reasoning effort from the metadata-only thread/resume read. These
// describe the spawn itself, so they are the one late write a settled spawn
// row accepts. Everything else on a metadata update (liveness, background
// flags, execution state) stays on the live projection.
// See docs/specs/agent-visibility.md#immutable-agent-history.
var codexSpawnIdentityInputKeys = []string{
	"newAgentNickname", "newAgentRole", "model", "reasoningEffort",
	"prompt", "agentPath", "taskName", "receiverAgents",
}

// codexSpawnIdentityMeta returns the spawn row meta with the identity
// fields from a metadata-only update merged into `input`, and false when
// the update adds nothing the row does not already carry.
func codexSpawnIdentityMeta(existingMeta string, incoming json.RawMessage) (string, bool) {
	var update struct {
		Input map[string]json.RawMessage `json:"input"`
	}
	if json.Unmarshal(incoming, &update) != nil || len(update.Input) == 0 {
		return "", false
	}
	identity := make(map[string]json.RawMessage, len(codexSpawnIdentityInputKeys))
	for _, key := range codexSpawnIdentityInputKeys {
		if value, ok := update.Input[key]; ok && !rawJSONEmpty(value) {
			identity[key] = value
		}
	}
	// The receiver list is spawn-time identity too, but a profile update
	// built for one child must never shrink a multi-child spawn's list.
	if len(decodeCodexItemMeta(json.RawMessage(existingMeta)).ReceiverThreadIDs) == 0 {
		if value, ok := update.Input["receiverThreadIds"]; ok && !rawJSONEmpty(value) {
			identity["receiverThreadIds"] = value
		}
	}
	if len(identity) == 0 {
		return "", false
	}
	encoded, err := json.Marshal(map[string]any{"input": identity})
	if err != nil {
		return "", false
	}
	merged := mergeItemMetaJSON(existingMeta, encoded)
	if merged == existingMeta {
		return "", false
	}
	return merged, true
}

func rawJSONEmpty(value json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(value))
	return trimmed == "" || trimmed == "null" || trimmed == `""` || trimmed == "[]" || trimmed == "{}"
}

// persistCodexSpawnIdentity writes identity onto a settled spawn row and
// republishes it. Status, timestamps, parentage and payloads are untouched;
// the write bypasses the settled-row guard in persistItemWithEmit on
// purpose, because that guard exists for execution state.
func (r *Router) persistCodexSpawnIdentity(existing store.Item, evt provider.ProviderEvent) error {
	merged, ok := codexSpawnIdentityMeta(existing.Meta, evt.Meta)
	if !ok {
		return nil
	}
	existing.Meta = merged
	inputPayload := r.shapeToolItemMeta(&existing, eventTimestampMillis(evt))
	persisted, err := r.store.UpsertItemWithInputPayload(existing, nil, inputPayload)
	if err != nil {
		return err
	}
	r.emitItemUpsert(persisted)
	return nil
}
