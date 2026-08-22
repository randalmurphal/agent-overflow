package sessionimport

import (
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
)

// One subagent transcript, on its own.
//
// LoadSubagents is the whole-session join: it discovers which agent
// belongs to which Task by walking a parent transcript's tool_result
// rows, because a subagent row carries no `parentToolUseID` and the
// file name is the only link. That is the right shape for import, which
// starts from a session file and has to find everything in it.
//
// The live path arrives from the other direction. Claude's
// `system/task_notification` hands out the agent's sidechain JSONL as
// `output_file` and names the launching `tool_use_id` in the same
// envelope, so the pair the join exists to discover is already known —
// and there is no parent transcript in play at all, only a live thread.
// These two entry points serve that caller: one file, one known launch,
// nothing else read.

// ConvertSubagentTranscript reads ONE subagent sidechain transcript
// (`<sessionDir>/subagents/agent-<agentId>.jsonl`, which is what a
// `task_notification` `output_file` resolves to for a `local_agent`
// task) and projects it into the import event vocabulary scoped to the
// tool_use that launched it.
//
// Every event comes back with `ParentToolUseID = launchToolUseID` and
// `TurnIndex` 0: a subagent has no turns of its own — its rows live in
// the launching turn (invariant 10) — so pinning the turn is the
// caller's job.
//
// The caller is responsible for bounding the file before calling: this
// reads it whole, the same way import reads a joined subagent.
func ConvertSubagentTranscript(path, launchToolUseID string) (ConvertResult, error) {
	if strings.TrimSpace(launchToolUseID) == "" {
		return ConvertResult{}, fmt.Errorf("sessionimport: convert subagent transcript: empty launch tool_use id")
	}
	rows, err := readSubagentRows(path)
	if err != nil {
		return ConvertResult{}, fmt.Errorf("sessionimport: read subagent transcript %q: %w", path, err)
	}
	return ConvertSubagentRows(rows, launchToolUseID), nil
}

// ConvertSubagentRows is ConvertSubagentTranscript's in-memory half: the
// same projection over rows the caller already holds.
//
// It runs the ordinary converter with `subagentScope` pinned from the
// start, which is exactly the state `emitSubagent` puts it in while
// converting a joined agent — so a sidechain converted on its own here
// produces the same events, in the same order, with the same ids, as the
// same rows converted as part of a full session import. Turn management
// stays suppressed for the same reason it is suppressed there (a
// subagent's rows belong to the launching turn), and there is therefore
// no open turn to close and no synthesised `EventTurnComplete`: the
// agent's usage belongs on its parent's turn, which this caller is not
// building.
func ConvertSubagentRows(rows []Row, launchToolUseID string) ConvertResult {
	scope := strings.TrimSpace(launchToolUseID)
	if scope == "" || len(rows) == 0 {
		return ConvertResult{}
	}
	c := &converter{
		usageByModel:  map[string]*provider.TokenUsage{},
		unknownSystem: map[string]int{},
		emittedAgents: map[string]bool{},
		subagentScope: scope,
	}
	c.indexCompactSummaries(rows)
	c.seedClock(rows)
	for _, row := range rows {
		c.convertRow(row)
	}
	c.appendDeferredWarnings()
	return ConvertResult{Events: c.events, Warnings: c.warnings}
}
