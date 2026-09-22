package sessionimport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/importir"
)

// agentFilePrefix is the naming rule for a subagent transcript:
// `<sessionDir>/subagents/agent-<agentId>.jsonl`.
const agentFilePrefix = "agent-"

// WarnMissingSubagent marks a Task whose subagent transcript is gone.
const WarnMissingSubagent = "subagent-missing"

// WarnUnreadableSubagent marks a transcript that exists but could not be read.
const WarnUnreadableSubagent = "subagent-unreadable"

// LoadSubagents joins each branch's Task/Agent tool calls to the subagent
// transcripts they spawned, keyed by the PARENT tool_use id.
//
// The join is the only one available: subagent rows carry no
// `parentToolUseID`. What they do have is a file name, and the parent
// transcript's Task tool_result carries `toolUseResult.agentId` — the
// same id. So the tool_result row that closes the Task both names the
// agent and identifies the launch its rows belong under.
//
// An unavailable file is a warning, never a parent-import failure: a Task
// whose transcript cannot be read still imports its launch and result rows.
func LoadSubagents(sessionDir string, branches []Branch) (map[string][]Row, []importir.Warning) {
	joins := collectAgentJoins(branches)
	if len(joins) == 0 {
		return nil, nil
	}

	out := make(map[string][]Row, len(joins))
	var (
		warnings   []importir.Warning
		missing    []string
		unreadable []string
	)
	for _, join := range joins {
		path, ok := subagentTranscriptPath(sessionDir, join.agentID)
		if !ok {
			unreadable = append(unreadable, fmt.Sprintf("%q: invalid subagent transcript identity", join.agentID))
			continue
		}
		rows, err := readSubagentRows(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				missing = append(missing, join.agentID)
			} else {
				unreadable = append(unreadable, fmt.Sprintf("%s: %v", join.agentID, err))
			}
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
	if len(unreadable) > 0 {
		warnings = append(warnings, importir.Warning{
			Code: WarnUnreadableSubagent,
			Message: fmt.Sprintf("%d subagent transcript(s) could not be read; their launch rows imported without nested detail. First error: %s",
				len(unreadable), unreadable[0]),
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
func readSubagentRows(path string) (rows []Row, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close subagent transcript %q: %w", path, closeErr))
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat subagent transcript %q: %w", path, err)
	}
	if err := eachSubagentFileRow(f, info.Size(), func(row Row) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		return nil, err
	}
	return rows, nil
}
