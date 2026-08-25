package sessionimport

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider/claude/sessionfork"
)

// agentFilePrefix is the naming rule for a subagent transcript:
// `<sessionDir>/subagents/agent-<agentId>.jsonl`.
const agentFilePrefix = "agent-"

// WarnMissingSubagent marks a Task whose subagent transcript is gone.
const WarnMissingSubagent = "subagent-missing"

// LoadSubagents joins each branch's Task/Agent tool calls to the subagent
// transcripts they spawned, keyed by the PARENT tool_use id.
//
// The join is the only one available: subagent rows carry no
// `parentToolUseID`. What they do have is a file name, and the parent
// transcript's Task tool_result carries `toolUseResult.agentId` — the
// same id. So the tool_result row that closes the Task both names the
// agent and identifies the launch its rows belong under.
//
// A missing file is a warning, never an error: a Task whose transcript
// was cleaned up still imported its launch and result rows.
func LoadSubagents(sessionDir string, branches []Branch) (map[string][]Row, []importir.Warning) {
	joins := collectAgentJoins(branches)
	if len(joins) == 0 {
		return nil, nil
	}

	out := make(map[string][]Row, len(joins))
	var (
		warnings []importir.Warning
		missing  []string
	)
	for _, join := range joins {
		path, ok := subagentTranscriptPath(sessionDir, join.agentID)
		if !ok {
			continue
		}
		rows, err := readSubagentRows(path)
		if err != nil {
			missing = append(missing, join.agentID)
			continue
		}
		if len(rows) > 0 {
			out[join.toolUseID] = rows
		}
	}
	if len(missing) > 0 {
		warnings = append(warnings, importir.Warning{
			Code:    WarnMissingSubagent,
			Message: fmt.Sprintf("%d subagent transcript(s) are no longer on disk; their launch rows imported without nested detail.", len(missing)),
		})
	}
	return out, warnings
}

type agentJoin struct {
	toolUseID string
	agentID   string
}

// collectAgentJoins pairs each agent with the tool call that launched it,
// in chain order.
//
// Both sides are claimed once. A tool call appears on several branches
// (they share a prefix), and an agent can be named by more than one
// result row (an async launch ack, then a resume ack) — binding an agent
// to the FIRST tool call that named it is what keeps its rows from
// nesting under two launches, and chain order makes "first" the launch
// rather than whichever id sorts lower.
func collectAgentJoins(branches []Branch) []agentJoin {
	var joins []agentJoin
	claimedAgents := map[string]bool{}
	claimedTools := map[string]bool{}
	for _, branch := range branches {
		for _, row := range branch.Chain {
			if row.Type != "user" {
				continue
			}
			agentID := strings.TrimSpace(rawString(rawMapValue(row.Raw["toolUseResult"]), "agentId"))
			if agentID == "" || claimedAgents[agentID] {
				continue
			}
			for _, block := range filterBlocks(contentBlocks(messageOf(row)), "tool_result") {
				toolUseID := rawString(block, "tool_use_id")
				if toolUseID == "" || claimedTools[toolUseID] {
					continue
				}
				claimedAgents[agentID] = true
				claimedTools[toolUseID] = true
				joins = append(joins, agentJoin{toolUseID: toolUseID, agentID: agentID})
				break
			}
		}
	}
	return joins
}

// subagentTranscriptPath builds the transcript path for an agent id,
// refusing any id that is not a single path-safe component. The id comes
// out of a file we do not write, so it must not be able to steer the read
// out of the session's own directory.
func subagentTranscriptPath(sessionDir, agentID string) (string, bool) {
	if sessionDir == "" || agentID == "" {
		return "", false
	}
	if agentID != filepath.Base(agentID) || strings.ContainsAny(agentID, `/\`) || strings.HasPrefix(agentID, ".") {
		return "", false
	}
	return filepath.Join(sessionDir, subagentsSubdir, agentFilePrefix+agentID+".jsonl"), true
}

// readSubagentRows reads one subagent transcript in file order.
//
// Unlike the main transcript this is NOT run through BuildBranches: a
// subagent transcript is a single linear run with no user-driven forking,
// so file order is the conversation. Progress rows are dropped for the
// same reason the DAG drops them — they are not content.
func readSubagentRows(path string) ([]Row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return readSubagentRowsFrom(f, sessionfork.SessionIDFromPath(path))
}

func readSubagentRowsFrom(reader io.Reader, sourceSessionID string) ([]Row, error) {
	entries, _, err := sessionfork.ParseTranscript(reader, sourceSessionID)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(entries))
	for _, row := range newRows(entries) {
		if row.Type == "progress" {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}
