package checkpoint

import (
	"encoding/json"

	"agent-overflow/internal/store"
)

// IsWireOnlyUserItem reports whether item is a wire-only user_text
// echo (a row triage synthesised from a Codex `userMessage` /
// Claude `replay-user-messages` envelope after the user had already
// withdrawn the live composer draft). Such rows carry `wire_only:
// true` on their persisted Meta JSON; the fork / revert / restore
// paths skip them because there is no checkpoint to anchor to —
// they were never preceded by a real user send.
//
// Decode failure returns false: a row whose Meta we can't parse is
// indistinguishable from one without the flag, and the safe default
// is to treat it as a normal user_text and let downstream guards
// catch the actual problem.
func IsWireOnlyUserItem(item store.Item) bool {
	if item.Meta == "" {
		return false
	}
	var meta map[string]any
	if json.Unmarshal([]byte(item.Meta), &meta) != nil {
		return false
	}
	wireOnly, _ := meta["wire_only"].(bool)
	return wireOnly
}
