package triage

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/workflowtools"
)

// aoResultTextMax bounds each string the stamp copies from a reply. The row
// shows these in one header line (frontend aoTools.ts WHAT_MAX), so nothing
// past it is ever painted, and every stamped value stays far under the wire
// projection's leaf floor.
const aoResultTextMax = 160

// aoToolResult is what a thread or workflow tool row shows of its reply
// without reading the reply's payload: the thread or run the call touched
// and where it stood when the call returned. The reply stays canonical; this
// is the header's copy (frontend aoTools.ts reads `meta.ao_result`).
type aoToolResult struct {
	// Thread tools.
	Token    string `json:"token,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
	Title    string `json:"title,omitempty"`
	Computer string `json:"computer,omitempty"`
	// Workflow tools.
	RunID      string `json:"run_id,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
	// Both.
	State   string `json:"state,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

func (r aoToolResult) empty() bool { return r == aoToolResult{} }

// StampAoToolResult records on a thread or workflow tool row what its reply
// says, for the row's header: `ao_result` (aoToolResult) for every such
// call, and for a thread_loop, thread_watch or thread_remind the next fire
// time (stampScheduledNextFire). Both providers stamp `meta.mcp` on an MCP
// tool row, so this reads no provider's wire, and the live completion and
// the history import call it alike. Any other row, and a failed call, is
// returned unchanged.
func StampAoToolResult(metaJSON, reply string, failed bool, rowAt int64) string {
	if failed || reply == "" {
		return metaJSON
	}
	var meta struct {
		MCP struct {
			Server string `json:"server"`
			Tool   string `json:"tool"`
		} `json:"mcp"`
	}
	if json.Unmarshal([]byte(metaJSON), &meta) != nil {
		return metaJSON
	}
	var result aoToolResult
	switch meta.MCP.Server {
	case threadtools.MCPServerName:
		metaJSON = stampScheduledNextFire(metaJSON, reply, false, rowAt)
		result = threadToolResult(reply)
	case workflowtools.MCPServerName:
		result = workflowToolResult(reply)
	default:
		return metaJSON
	}
	if result.empty() {
		return metaJSON
	}
	encoded, err := json.Marshal(map[string]aoToolResult{"ao_result": result})
	if err != nil {
		return metaJSON
	}
	return mergeItemMetaJSON(metaJSON, encoded)
}

// firstJSONValue decodes the reply's first JSON value. A client may append
// more after it (Codex adds structured content).
func firstJSONValue(reply string, target any) bool {
	return json.NewDecoder(strings.NewReader(reply)).Decode(target) == nil
}

// threadToolResult reads a thread tools reply (internal/threadtools): an
// ack's token, target and outcome, a read's thread, a cancel's state, a
// reply's sender.
func threadToolResult(reply string) aoToolResult {
	var r struct {
		Token          string `json:"token"`
		ThreadID       string `json:"thread_id"`
		SourceThreadID string `json:"source_thread_id"`
		Title          string `json:"title"`
		Computer       string `json:"computer"`
		SourceComputer string `json:"source_computer"`
		State          string `json:"state"`
		Outcome        string `json:"outcome"`
	}
	if !firstJSONValue(reply, &r) {
		return aoToolResult{}
	}
	threadID := r.ThreadID
	if threadID == "" {
		threadID = r.SourceThreadID
	}
	computer := r.Computer
	if computer == "" {
		computer = r.SourceComputer
	}
	return aoToolResult{
		Token:    clipResultText(r.Token),
		ThreadID: clipResultText(threadID),
		Title:    clipResultText(r.Title),
		Computer: clipResultText(computer),
		State:    clipResultText(r.State),
		Outcome:  clipResultText(r.Outcome),
	}
}

// workflowRunRef is the part of a workflow run document the stamp reads.
type workflowRunRef struct {
	ItemID     string `json:"itemId"`
	WorkflowID string `json:"workflowId"`
	State      string `json:"state"`
}

// workflowToolResult reads a workflow tools reply (internal/workflowtools):
// run_start's start document, a waiting call's run document and outcome, or
// a bare run document (run_output, run_amend).
func workflowToolResult(reply string) aoToolResult {
	var r struct {
		workflowRunRef
		Start   *workflowRunRef `json:"start"`
		Run     *workflowRunRef `json:"run"`
		Outcome string          `json:"outcome"`
	}
	if !firstJSONValue(reply, &r) {
		return aoToolResult{}
	}
	run := r.workflowRunRef
	// The run document is the newer read: run_start's wait reads the run
	// after the start document was written.
	for _, ref := range []*workflowRunRef{r.Start, r.Run} {
		if ref == nil {
			continue
		}
		if ref.ItemID != "" {
			run.ItemID = ref.ItemID
		}
		if ref.WorkflowID != "" {
			run.WorkflowID = ref.WorkflowID
		}
		if ref.State != "" {
			run.State = ref.State
		}
	}
	return aoToolResult{
		RunID:      clipResultText(run.ItemID),
		WorkflowID: clipResultText(run.WorkflowID),
		State:      clipResultText(run.State),
		Outcome:    clipResultText(r.Outcome),
	}
}

func clipResultText(text string) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= aoResultTextMax {
		return text
	}
	runes := []rune(text)
	return string(runes[:aoResultTextMax-1]) + "…"
}
