// Tray-only metadata decorated onto a read-time Codex launch copy. Keep these
// values mirrored with internal/store/subagent_items.go; mirror_pins_test.go
// enforces the cross-language contract.
export const CODEX_LATEST_TOOL_META = {
  summary: 'subagentLatestToolSummary',
  turnIndex: 'subagentLatestToolTurnIndex',
  itemIndex: 'subagentLatestToolItemIndex',
} as const;
