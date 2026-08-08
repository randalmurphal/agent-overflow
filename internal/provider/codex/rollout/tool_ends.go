package rollout

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
)

// End records — `exec_command_end`, `patch_apply_end`, `mcp_tool_call_end`,
// `web_search_end`, and the four collab `*_end` variants. Each names a
// `call_id` and carries the result detail the matching `*_output` response
// item does not. They normally arrive BEFORE that output line, so an end
// record enriches the open call and the output line finishes it; when the
// call is unknown to this file, a self-contained end record still stands as
// its own row rather than being dropped.

// endEvent is one end record, decoded independently of whether the call it
// names is known to this file.
type endEvent struct {
	callID   string
	toolName string
	itemType string
	input    map[string]any
	what     string // prose naming the record in a warning
	enrich   toolEnrichment
}

// selfContained reports whether the end record carries enough on its own to
// stand as a row — either a result, or an invocation we can name.
//
// `patch_apply_end` is the case that matters: a patch applied from inside an
// `exec` script is stamped with a synthetic `exec-<uuid>` call id that appears
// nowhere else in the file, yet the record still holds the complete diff.
// Turning that into an "unavailable" placeholder would throw away the edit,
// and it is the majority of all file changes in a modern rollout. Only a
// record with neither a result nor an identifiable invocation degrades.
func (e endEvent) selfContained() bool {
	return e.enrich.diffPatch != "" || e.enrich.output != "" || e.enrich.exitCode != nil || len(e.input) > 0
}

// applyEndEvent routes an end record: onto its tool call when the wire's
// `call_id` names one, as its own row when the record stands alone, and as
// the `import_unavailable` marker only when there is genuinely nothing left
// to show.
func (c *converter) applyEndEvent(ev endEvent) {
	ev.callID = strings.TrimSpace(ev.callID)
	if tool, ok := c.tools[ev.callID]; ok && ev.callID != "" {
		tool.enrich = mergeEnrichment(tool.enrich, ev.enrich)
		return
	}
	if ev.selfContained() {
		c.emitStandaloneTool(ev)
		return
	}
	c.emitOrphanCompletion(ev.callID, ev.what)
}

func mergeEnrichment(into *toolEnrichment, from toolEnrichment) *toolEnrichment {
	if into == nil {
		merged := from
		if merged.extra == nil {
			merged.extra = map[string]any{}
		}
		return &merged
	}
	if from.command != "" {
		into.command = from.command
	}
	if from.cwd != "" {
		into.cwd = from.cwd
	}
	if from.exitCode != nil {
		into.exitCode = from.exitCode
	}
	if from.isError {
		into.isError = true
	}
	if from.itemStatus != "" {
		into.itemStatus = from.itemStatus
	}
	if from.output != "" {
		into.output = from.output
	}
	if from.diffPatch != "" {
		into.diffPatch = from.diffPatch
	}
	for k, v := range from.extra {
		into.extra[k] = v
	}
	return into
}

// emitStandaloneTool turns a self-contained end record into its own
// launch/completion pair, positioned where it happened in the file.
func (c *converter) emitStandaloneTool(ev endEvent) {
	c.ensureTurn()
	callID := ev.callID
	if callID == "" {
		callID = lineUUID(c.lineStart)
	}
	input := ev.input
	if input == nil {
		input = map[string]any{}
	}
	if ev.enrich.command != "" {
		input["command"] = ev.enrich.command
	}
	if ev.enrich.cwd != "" {
		input["cwd"] = ev.enrich.cwd
	}
	tool := &openTool{
		callID:    callID,
		itemID:    callID,
		itemType:  ev.itemType,
		toolName:  ev.toolName,
		command:   ev.enrich.command,
		turnID:    c.turn.id,
		turnIndex: c.turn.index,
		startedAt: c.lastTimestamp,
		enrich:    mergeEnrichment(nil, ev.enrich),
	}
	if len(input) > 0 {
		if encoded, err := json.Marshal(input); err == nil {
			tool.input = encoded
		}
	}
	c.emit(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		TurnID:    tool.turnID,
		ItemID:    tool.itemID,
		ItemType:  tool.itemType,
		Role:      "assistant",
		Meta:      c.toolStartMeta(tool),
		Timestamp: c.lastTimestamp,
	})
	c.finishTool(tool, "", "", false)
}

// emitOrphanCompletion records an end event that names no known call AND
// carries nothing usable of its own.
//
// That happens when the call is outside the imported range: a fork's
// inherited prefix, a compaction that replaced the window, or a tail refresh
// starting after the call. The row is created with no payload and the
// `import_unavailable` marker the frontend renders as "Not available from
// import" — a visible gap beats a silently missing tool call.
func (c *converter) emitOrphanCompletion(callID, what string) {
	if callID == "" {
		callID = lineUUID(c.lineStart)
	}
	c.ensureTurn()
	c.emit(provider.ProviderEvent{
		Kind:     provider.EventToolComplete,
		ItemID:   callID,
		ItemType: "toolCall",
		Meta: metaJSON(map[string]any{
			MetaImportUnavailableKey: MetaImportUnavailableExecDetail,
			"item_status":            "completed",
		}),
		Timestamp: c.lastTimestamp,
	})
	c.warnings = append(c.warnings, importir.Warning{
		Code:    WarnUnmatchedEnd,
		Message: fmt.Sprintf("A Codex %s (%s) had no matching tool call in this session file; its detail is unavailable.", what, callID),
	})
}

// applyExecCommandEnd folds an `exec_command_end` into the pending
// completion.
//
// The event is only present in rollouts written before Codex stopped
// persisting it (it is listed under the not-persisted arm of
// codex-rs/rollout/src/policy.rs today), which is why it is the ONLY source
// of a command exit code on the import path — modern `*_output` lines carry
// no status field at all.
func (c *converter) applyExecCommandEnd(env envelope) {
	var p execCommandEndPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	ev := endEvent{
		callID:   p.CallID,
		toolName: "Bash",
		itemType: "commandExecution",
		what:     "command result",
		enrich: toolEnrichment{
			cwd:      p.Cwd,
			exitCode: p.ExitCode,
			isError:  p.ExitCode != nil && *p.ExitCode != 0,
			output:   firstNonEmpty(p.AggregatedOutput, p.Stdout, p.Stderr),
			extra:    map[string]any{},
		},
	}
	if len(p.Command) > 0 {
		ev.enrich.command = strings.Join(p.Command, " ")
	}
	if p.Source != "" {
		ev.enrich.extra["source"] = p.Source
	}
	c.applyEndEvent(ev)
}

// applyPatchApplyEnd folds a `patch_apply_end` into the pending completion
// and assembles its per-file hunks into one unified patch.
func (c *converter) applyPatchApplyEnd(env envelope) {
	var p patchApplyEndPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	ev := endEvent{
		callID:   p.CallID,
		toolName: "file_change",
		itemType: "fileChange",
		what:     "patch result",
		enrich: toolEnrichment{
			output:    firstNonEmpty(p.Stdout, p.Stderr),
			diffPatch: assembleUnifiedPatch(p.Changes),
			extra:     map[string]any{},
		},
	}
	if p.Success != nil && !*p.Success {
		ev.enrich.isError = true
		ev.enrich.itemStatus = "failed"
	}
	if paths := changedPaths(p.Changes); len(paths) > 0 {
		ev.enrich.extra["files"] = paths
		ev.input = map[string]any{}
		if len(paths) == 1 {
			ev.input["file_path"] = paths[0]
		} else {
			ev.input["files"] = paths
		}
	}
	c.applyEndEvent(ev)
}

// assembleUnifiedPatch turns Codex's per-path `unified_diff` hunks into one
// patch with the `--- a/… +++ b/…` headers every downstream diff reader (and
// triage.ExtractDiffMeta) anchors on. Codex records only the hunks.
func assembleUnifiedPatch(changes map[string]json.RawMessage) string {
	if len(changes) == 0 {
		return ""
	}
	paths := changedPaths(changes)
	var b strings.Builder
	for _, path := range paths {
		var change struct {
			Type        string `json:"type"`
			UnifiedDiff string `json:"unified_diff"`
			MovePath    string `json:"move_path"`
		}
		if json.Unmarshal(changes[path], &change) != nil || change.UnifiedDiff == "" {
			continue
		}
		newPath := path
		if change.MovePath != "" {
			newPath = change.MovePath
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("diff --git a/" + path + " b/" + newPath + "\n")
		switch change.Type {
		case "add":
			b.WriteString("new file\n")
		case "delete":
			b.WriteString("deleted file\n")
		}
		b.WriteString("--- a/" + path + "\n")
		b.WriteString("+++ b/" + newPath + "\n")
		b.WriteString(strings.TrimRight(change.UnifiedDiff, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

func changedPaths(changes map[string]json.RawMessage) []string {
	paths := make([]string, 0, len(changes))
	for path := range changes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func (c *converter) applyMCPToolCallEnd(env envelope) {
	var p mcpToolCallEndPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	ev := endEvent{
		callID:   p.CallID,
		toolName: "mcp_tool_call",
		itemType: "mcpToolCall",
		what:     "MCP tool result",
		enrich:   toolEnrichment{extra: map[string]any{}},
	}
	if p.Invocation != nil {
		if p.Invocation.Server != "" {
			ev.enrich.extra["mcpServer"] = p.Invocation.Server
			ev.toolName = p.Invocation.Server
		}
		if p.Invocation.Tool != "" {
			ev.enrich.extra["mcpTool"] = p.Invocation.Tool
			ev.toolName = p.Invocation.Server + "__" + p.Invocation.Tool
		}
		if len(p.Invocation.Arguments) > 0 {
			ev.input = map[string]any{"arguments": json.RawMessage(p.Invocation.Arguments)}
		}
	}
	if raw, ok := p.Result["Err"]; ok && len(raw) > 0 {
		ev.enrich.isError = true
		ev.enrich.itemStatus = "failed"
		ev.enrich.output, _ = contentText(raw)
	} else if raw, ok := p.Result["Ok"]; ok && len(raw) > 0 {
		var result struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &result) == nil {
			ev.enrich.output, _ = contentText(result.Content)
		}
	}
	c.applyEndEvent(ev)
}

func (c *converter) applyWebSearchEnd(env envelope) {
	var p webSearchEndPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	ev := endEvent{
		callID:   p.CallID,
		toolName: "web_search",
		itemType: "webSearch",
		what:     "web search result",
		enrich:   toolEnrichment{extra: map[string]any{}},
	}
	if p.Query != "" {
		ev.enrich.extra["query"] = p.Query
		ev.input = map[string]any{"query": p.Query}
	}
	c.applyEndEvent(ev)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
