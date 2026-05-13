// Package codexghost holds the pure summary-rewrite helpers used by
// the Codex ghost-row flip (the rule that on every Codex session start
// transitions persisted background tool_call rows from a now-dead
// subprocess into the timeline's `errored` / `lost` state).
package codexghost
