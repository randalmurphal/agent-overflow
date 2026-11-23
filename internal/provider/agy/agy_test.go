package agy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
	"agent-overflow/internal/triage"
)

func TestAgyFlavorUsageToolsAndMCPTranslation(t *testing.T) {
	t.Parallel()
	f := NewFlavor("")

	// Gemini usage math: inputTokens is already non-cached, outputTokens already includes thoughtTokens.
	u := f.ConvertUsage(acp.Usage{
		InputTokens:       7747,
		OutputTokens:      1071,
		CachedReadTokens:  22219,
		CachedWriteTokens: 0,
		ThoughtTokens:     904,
		TotalTokens:       31037,
	}, "gemini-4-neon")
	if u.InputTokens != 7747 || u.CacheReadInputTokens != 22219 || u.OutputTokens != 1071 || u.ReasoningOutputTokens != 904 {
		t.Fatalf("unexpected usage conversion: %+v", u)
	}

	// System prompt append meta.
	meta := f.BuildSessionMeta(acp.SessionConfig{AppendSystemPrompt: "AO guide"})
	ag, _ := meta["antigravity"].(map[string]any)
	sp, _ := ag["systemPrompt"].(map[string]any)
	if sp["append"] != "AO guide" {
		t.Fatalf("unexpected session meta: %+v", meta)
	}

	// Tool normalization: read_file prefers locations[0].path, write_file plan artifact -> ProposedPlan.
	readNorm := f.NormalizeToolStart(&acp.ToolCallUpdate{
		Name:      "read_file",
		Title:     "File ../../../../tmp/foo.go",
		Locations: []acp.ToolLocation{{Path: "/abs/foo.go"}},
	})
	if readNorm.ToolName != "Read" || !strings.Contains(string(readNorm.Input), "/abs/foo.go") {
		t.Fatalf("unexpected read_file norm: %+v input=%s", readNorm, readNorm.Input)
	}

	planComp := f.NormalizeToolComplete(&acp.ToolCallUpdate{
		Name:     "write_file",
		Status:   "completed",
		RawInput: json.RawMessage(`{"Path":"plan.md","Content":"# My Plan","ArtifactMetadata":{"RequestFeedback":true,"UserFacing":true}}`),
	}, acp.NormalizedToolStart{ToolName: "Write", ItemType: "file_change"})
	if planComp.ProposedPlan != "# My Plan" {
		t.Fatalf("expected ProposedPlan, got %+v", planComp)
	}

	// Subagent launch tagging.
	subNorm := f.NormalizeToolStart(&acp.ToolCallUpdate{
		Name:     "invoke_subagent",
		RawInput: json.RawMessage(`{"TypeName":"research","Role":"Code Auditor","Prompt":"check it"}`),
	})
	if !subNorm.SubagentLaunch || subNorm.SubagentType != "research" || subNorm.Description != "Code Auditor" {
		t.Fatalf("unexpected subagent norm: %+v", subNorm)
	}

	// Security warning surfacing on allow_always option.
	cls := f.ClassifyPermission("t1", "turn-1", "req-1", acp.RequestPermissionParams{
		ToolCall: acp.ToolCallUpdate{ToolCallID: "c1", Name: "run_command", Kind: "execute", Title: "rm -rf"},
		Options: []acp.PermissionOption{
			{OptionID: "allow_always", Name: "Always Allow", Kind: "allow_always", Meta: json.RawMessage(`{"agy.security.warning":{"title":"Caution","message":"Broad shell grant"}}`)},
		},
	}, acp.PolicyPrompt)
	if !strings.Contains(cls.Approval.Description, "Broad shell grant") {
		t.Fatalf("expected security warning in description, got %q", cls.Approval.Description)
	}

	// MCP translation.
	translated := TranslateMCPServers(map[string]any{
		"ao-thread-tools": map[string]any{
			"url":     "http://127.0.0.1:4321/mcp",
			"headers": map[string]string{"Authorization": "Bearer tok"},
		},
	})
	if len(translated) != 1 || translated[0].Type != "http" || translated[0].Name != "ao-thread-tools" || len(translated[0].Headers) != 1 {
		t.Fatalf("unexpected translated MCP servers: %+v", translated)
	}
}

func seedFakeConversationDB(t *testing.T, appDataDir, sessionID string) string {
	t.Helper()
	dir := filepath.Join(appDataDir, "conversations")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	stmts := []string{
		`CREATE TABLE trajectory_meta (trajectory_id TEXT, cascade_id TEXT)`,
		`CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, error_details BLOB, step_payload BLOB)`,
		`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB)`,
		`CREATE TABLE executor_metadata (idx INTEGER PRIMARY KEY, data BLOB)`,
		`INSERT INTO trajectory_meta (trajectory_id, cascade_id) VALUES ('orig-traj', '` + sessionID + `')`,
		`INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, 'turn 0 user')`,
		`INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 15, 'turn 0 reply')`,
		`INSERT INTO steps (idx, step_type, step_payload) VALUES (2, 14, 'turn 1 user')`,
		`INSERT INTO steps (idx, step_type, step_payload) VALUES (3, 15, 'turn 1 reply')`,
		`INSERT INTO gen_metadata (idx, data) VALUES (1, 'g1'), (3, 'g3')`,
		`INSERT INTO executor_metadata (idx, data) VALUES (1, 'e1'), (3, 'e3')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %s: %v", s, err)
		}
	}
	return path
}

func TestTurnOutcomeDetectsStepType17Errors(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	sid := "sess-err-1"
	dbPath := seedFakeConversationDB(t, appDataDir, sid)

	f := NewFlavor(appDataDir)
	before := f.BeforePrompt(context.Background(), sid)
	if before != 3 {
		t.Fatalf("BeforePrompt maxIdx = %v, want 3", before)
	}
	if override := f.AfterPrompt(context.Background(), sid, "end_turn", before); override != nil {
		t.Fatalf("expected nil override before error step, got %+v", override)
	}

	// Append step_type = 17 quota error.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, error_details) VALUES (4, 17, ?)`, []byte("\x0a\x1eQUOTA_EXHAUSTED: try again later")); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	override := f.AfterPrompt(context.Background(), sid, "end_turn", before)
	if override == nil || override.Failure == nil || override.Failure.Reason != provider.FailureReasonUsageLimit {
		t.Fatalf("expected usage-limit override, got %+v", override)
	}
}

func TestForkConversationSlicesAndVerifiesSchema(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	srcID := "sess-src-1"
	seedFakeConversationDB(t, appDataDir, srcID)

	cutIdx, hasPrefix, err := FindCutIndexBeforeUserTurn(appDataDir, srcID, 1)
	if err != nil || !hasPrefix || cutIdx != 1 {
		t.Fatalf("FindCutIndexBeforeUserTurn = (%d, %v, %v), want (1, true, nil)", cutIdx, hasPrefix, err)
	}

	dstID, cleanup, err := ForkConversation(appDataDir, srcID, cutIdx)
	if err != nil {
		t.Fatalf("ForkConversation: %v", err)
	}
	defer func() { _ = cleanup() }()

	dstPath, err := conversationDBPath(appDataDir, dstID)
	if err != nil {
		t.Fatalf("conversationDBPath(dst): %v", err)
	}
	dstDB, err := sql.Open("sqlite", dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dstDB.Close() }()

	var count int
	if err := dstDB.QueryRow(`SELECT COUNT(*) FROM steps`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("forked steps count = %d (err=%v), want 2", count, err)
	}
	var cascadeID, trajID string
	if err := dstDB.QueryRow(`SELECT trajectory_id, cascade_id FROM trajectory_meta`).Scan(&trajID, &cascadeID); err != nil {
		t.Fatal(err)
	}
	if cascadeID != dstID || trajID == "orig-traj" {
		t.Fatalf("trajectory_meta not rewritten: traj=%q cascade=%q dst=%q", trajID, cascadeID, dstID)
	}

	// Schema guard must reject an incompatible DB without producing a corrupt clone.
	badID := "sess-bad-schema"
	badPath, err := conversationDBPath(appDataDir, badID)
	if err != nil {
		t.Fatalf("conversationDBPath(bad): %v", err)
	}
	badDB, err := sql.Open("sqlite", badPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = badDB.Exec(`CREATE TABLE other_table (id INTEGER)`)
	_ = badDB.Close()

	if _, _, err := ForkConversation(appDataDir, badID, -1); err == nil || !strings.Contains(err.Error(), "unsupported conversation schema") {
		t.Fatalf("expected schema guard refusal, got %v", err)
	}
}

func TestProbeCatalogZeroTokenModelMatrix(t *testing.T) {
	t.Parallel()
	script := `#!/usr/bin/env python3
import json, sys, time

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

model = "gemini-4-neon"
last_size = None
matrix = {
    "gemini-4-neon": (1048576, "high", [("low","Low"),("medium","Medium"),("high","High"),("xhigh","xHigh")]),
    "claude-opus-5-5": (1000000, "medium", [("low","Low"),("medium","Medium"),("high","High")]),
    "claude-sonnet-5-5": (1000000, "medium", [("low","Low"),("medium","Medium"),("high","High")]),
    "gpt-oss-120b-medium": (131072, "", []),
}

def opts(m):
    _, cur_t, ts = matrix[m]
    out = [{
        "id": "model", "name": "Model", "type": "select", "currentValue": m,
        "options": [
            {"value": "gemini-4-neon", "name": "Gemini 4 Neon"},
            {"value": "claude-opus-5-5", "name": "Claude Opus 5.5"},
            {"value": "claude-sonnet-5-5", "name": "Claude Sonnet 5.5"},
            {"value": "gpt-oss-120b-medium", "name": "GPT-OSS 120B (Medium)"},
        ],
    }]
    if ts:
        out.append({
            "id": "thought_level", "name": "Thought", "type": "select", "currentValue": cur_t,
            "options": [{"value": v, "name": n} for v, n in ts],
        })
    return out

for line in sys.stdin:
    line = line.strip()
    if not line: continue
    msg = json.loads(line)
    mid = msg.get("id")
    m = msg.get("method")
    p = msg.get("params") or {}
    if m == "initialize":
        send({"jsonrpc":"2.0","id":mid,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"Fake Agy","version":"1.5.14"}}})
    elif m == "session/new":
        send({"jsonrpc":"2.0","id":mid,"result":{"sessionId":"probe-1","configOptions":opts(model)}})
        last_size = matrix[model][0]
        send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"probe-1","update":{"sessionUpdate":"usage_update","used":0,"size":last_size}}})
    elif m == "session/set_config_option":
        model = p["value"]
        # Mirror real agy 1.5.14 wire order: reply first, then emit usage_update
        # asynchronously only when the context window size actually changes.
        send({"jsonrpc":"2.0","id":mid,"result":{"configOptions":opts(model)}})
        new_size = matrix[model][0]
        if new_size != last_size:
            last_size = new_size
            time.sleep(0.015)
            send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"probe-1","update":{"sessionUpdate":"usage_update","used":0,"size":new_size}}})
    elif m == "session/close":
        send({"jsonrpc":"2.0","id":mid,"result":{}})
`
	bin := filepath.Join(t.TempDir(), "fake-agy-catalog.py")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	models, err := ProbeCatalog(context.Background(), bin, t.TempDir(), nil, procutiltest.Reaper())
	if err != nil {
		t.Fatalf("ProbeCatalog: %v", err)
	}
	if len(models) != 4 {
		t.Fatalf("len(models) = %d, want 4: %+v", len(models), models)
	}
	if models[0].Slug != "gemini-4-neon" || !models[0].IsDefault || len(models[0].ReasoningEfforts) != 4 || models[0].ContextWindows[0].Tokens != 1048576 {
		t.Fatalf("unexpected model 0: %+v", models[0])
	}
	if models[1].Slug != "claude-opus-5-5" || models[1].ContextWindows[0].Tokens != 1000000 {
		t.Fatalf("unexpected model 1: %+v", models[1])
	}
	if models[2].Slug != "claude-sonnet-5-5" || models[2].ContextWindows[0].Tokens != 1000000 {
		t.Fatalf("unexpected model 2: %+v", models[2])
	}
	if models[3].Slug != "gpt-oss-120b-medium" || len(models[3].ReasoningEfforts) != 0 || models[3].ContextWindows[0].Tokens != 131072 {
		t.Fatalf("unexpected model 3: %+v", models[3])
	}
}

func TestAgyFlavorRichToolAndMCPMetadata(t *testing.T) {
	t.Parallel()
	f := NewFlavor("")

	// 1. MCP tools (AO hyphenated/underscored + generic + double-underscore) stamp meta.mcp + MCP/<tool>.
	mcpCases := []struct {
		wire, server, tool string
	}{
		{"mcp_ao-thread-tools_spawn_thread", "ao-thread-tools", "spawn_thread"},
		{"mcp_ao_browser_tools_click", "ao-browser-tools", "click"},
		{"mcp__github__list_prs", "github", "list_prs"},
		{"mcp_postgres_run_query", "postgres", "run_query"},
	}
	for _, tc := range mcpCases {
		norm := f.NormalizeToolStart(&acp.ToolCallUpdate{
			Name:     tc.wire,
			RawInput: json.RawMessage(`{"x":1}`),
		})
		if norm.ToolName != "MCP/"+tc.tool {
			t.Errorf("%s: ToolName = %q, want %q", tc.wire, norm.ToolName, "MCP/"+tc.tool)
		}
		mcp, _ := norm.ExtraMeta["mcp"].(map[string]string)
		if mcp["server"] != tc.server || mcp["tool"] != tc.tool {
			t.Errorf("%s: meta.mcp = %+v, want server=%q tool=%q", tc.wire, mcp, tc.server, tc.tool)
		}
	}

	// 2. read_file preserves offset and limit.
	readNorm := f.NormalizeToolStart(&acp.ToolCallUpdate{
		Name:     "read_file",
		RawInput: json.RawMessage(`{"Path":"/repo/main.go","StartLine":10,"EndLine":29}`),
	})
	var readIn map[string]any
	if err := json.Unmarshal(readNorm.Input, &readIn); err != nil {
		t.Fatal(err)
	}
	if readIn["file_path"] != "/repo/main.go" || readIn["offset"] != float64(10) || readIn["limit"] != float64(20) {
		t.Fatalf("unexpected read input: %+v", readIn)
	}

	// 3. edit_file preserves old_string/new_string and synthesizes DiffChanges fallback on complete.
	editUpdate := &acp.ToolCallUpdate{
		Name:     "edit_file",
		Status:   "completed",
		RawInput: json.RawMessage(`{"Path":"/repo/a.go","Target":"old","Replacement":"new"}`),
	}
	editStart := f.NormalizeToolStart(editUpdate)
	if editStart.ToolName != "Edit" || editStart.ItemType != "file_change" ||
		!strings.Contains(string(editStart.Input), `"old_string":"old"`) ||
		!strings.Contains(string(editStart.Input), `"new_string":"new"`) {
		t.Fatalf("unexpected editStart: %+v input=%s", editStart, editStart.Input)
	}
	editComp := f.NormalizeToolComplete(editUpdate, editStart)
	if len(editComp.DiffChanges) != 1 || editComp.DiffChanges[0].Kind != "update" || !strings.Contains(editComp.DiffChanges[0].Diff, "+new") {
		t.Fatalf("expected synthesized edit DiffChanges, got %+v", editComp.DiffChanges)
	}

	// 4. grep_search, find_by_name, search_web, read_url_content, run_command, invoke_subagent, send_message, generate_image.
	grepNorm := f.NormalizeToolStart(&acp.ToolCallUpdate{
		Name:     "grep_search",
		RawInput: json.RawMessage(`{"Query":"PlanLiveUpdate","SearchPath":"/repo","Includes":["*.go"],"CaseInsensitive":true}`),
	})
	if grepNorm.ToolName != "Grep" || !strings.Contains(string(grepNorm.Input), `"pattern":"PlanLiveUpdate"`) || !strings.Contains(string(grepNorm.Input), `"-i":true`) {
		t.Fatalf("unexpected grepNorm: %+v input=%s", grepNorm, grepNorm.Input)
	}

	globNorm := f.NormalizeToolStart(&acp.ToolCallUpdate{
		Name:     "find_by_name",
		RawInput: json.RawMessage(`{"Pattern":"*.svelte","SearchDirectory":"/repo/frontend"}`),
	})
	if globNorm.ToolName != "Glob" || !strings.Contains(string(globNorm.Input), `"pattern":"*.svelte"`) {
		t.Fatalf("unexpected globNorm: %+v input=%s", globNorm, globNorm.Input)
	}

	webNorm := f.NormalizeToolStart(&acp.ToolCallUpdate{
		Name:     "search_web",
		RawInput: json.RawMessage(`{"query":"golang acp","domain":"go.dev"}`),
	})
	if webNorm.ToolName != "WebSearch" || !strings.Contains(string(webNorm.Input), `"allowed_domains":["go.dev"]`) {
		t.Fatalf("unexpected webNorm: %+v input=%s", webNorm, webNorm.Input)
	}

	fetchNorm := f.NormalizeToolStart(&acp.ToolCallUpdate{
		Name:     "read_url_content",
		RawInput: json.RawMessage(`{"Url":"https://example.com"}`),
	})
	if fetchNorm.ToolName != "WebFetch" || !strings.Contains(string(fetchNorm.Input), `"url":"https://example.com"`) {
		t.Fatalf("unexpected fetchNorm: %+v input=%s", fetchNorm, fetchNorm.Input)
	}

	cmdNorm := f.NormalizeToolStart(&acp.ToolCallUpdate{
		Name:     "run_command",
		RawInput: json.RawMessage(`{"CommandLine":"make test","toolSummary":"Run tests","Cwd":"/repo","IsDaemon":true,"WaitMsBeforeAsync":5000}`),
	})
	if cmdNorm.ToolName != "Bash" || !cmdNorm.IsBackground || cmdNorm.ExtraMeta["command"] != "make test" || cmdNorm.ExtraMeta["cwd"] != "/repo" {
		t.Fatalf("unexpected cmdNorm: %+v input=%s", cmdNorm, cmdNorm.Input)
	}

	subStart := f.NormalizeToolStart(&acp.ToolCallUpdate{
		Name:     "invoke_subagent",
		RawInput: json.RawMessage(`{"TypeName":"research","Role":"Auditor","Prompt":"audit","Model":"pro"}`),
	})
	subComp := f.NormalizeToolComplete(&acp.ToolCallUpdate{
		Name:      "invoke_subagent",
		Status:    "completed",
		RawOutput: json.RawMessage(`{"conversationId":"child-conv-42"}`),
	}, subStart)
	if subComp.ExtraMeta["agent_id"] != "child-conv-42" {
		t.Fatalf("expected agent_id on subagent complete, got %+v", subComp.ExtraMeta)
	}

	msgNorm := f.NormalizeToolStart(&acp.ToolCallUpdate{
		Name:     "send_message",
		RawInput: json.RawMessage(`{"Recipient":"child-conv-42","Message":"continue"}`),
	})
	if msgNorm.ToolName != "SendMessage" || !strings.Contains(string(msgNorm.Input), `"to":"child-conv-42"`) {
		t.Fatalf("unexpected msgNorm: %+v input=%s", msgNorm, msgNorm.Input)
	}

	imgNorm := f.NormalizeToolStart(&acp.ToolCallUpdate{
		Name:     "generate_image",
		RawInput: json.RawMessage(`{"Prompt":"logo","ImageName":"logo.png"}`),
	})
	if imgNorm.ToolName != "ImageGeneration" || !strings.Contains(string(imgNorm.Input), `"path":"logo.png"`) {
		t.Fatalf("unexpected imgNorm: %+v input=%s", imgNorm, imgNorm.Input)
	}
}

func TestAgyTransferSnapshotCopyAndValidate(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	srcID := "11111111-2222-3333-4444-555555555555"
	seedFakeConversationDB(t, appDataDir, srcID)

	// Add a brain artifact file to ensure companion files ride along.
	artifactPath := filepath.Join(appDataDir, "brain", srcID, "plan.md")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("# Plan"), 0o600); err != nil {
		t.Fatal(err)
	}

	scratchDir := t.TempDir()
	sources, err := TransferFiles(context.Background(), appDataDir, srcID, scratchDir)
	if err != nil {
		t.Fatalf("TransferFiles: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("len(sources) = %d, want 2: %+v", len(sources), sources)
	}
	if sources[0].Name != "native/agy/conversations/"+srcID+".db" {
		t.Fatalf("sources[0].Name = %q", sources[0].Name)
	}
	if sources[1].Name != "native/agy/brain/"+srcID+"/plan.md" {
		t.Fatalf("sources[1].Name = %q", sources[1].Name)
	}

	// CopyTransferFiles mints a deterministic UUID, rewrites trajectory_meta, and remaps brain/.
	copyDir := t.TempDir()
	copied1, err := CopyTransferFiles(context.Background(), "op-123", srcID, copyDir, sources)
	if err != nil {
		t.Fatalf("CopyTransferFiles: %v", err)
	}
	copied2, err := CopyTransferFiles(context.Background(), "op-123", srcID, copyDir, sources)
	if err != nil {
		t.Fatalf("CopyTransferFiles retry: %v", err)
	}
	if copied1.SessionID == srcID || copied1.SessionID != copied2.SessionID {
		t.Fatalf("expected deterministic fresh UUID, got src=%q copy1=%q copy2=%q", srcID, copied1.SessionID, copied2.SessionID)
	}
	if len(copied1.Files) != 2 || copied1.Files[1].Name != "native/agy/brain/"+copied1.SessionID+"/plan.md" {
		t.Fatalf("unexpected copied files: %+v", copied1.Files)
	}

	// Stage the copied files and verify ValidateStagedTransferFiles passes for new ID and rejects wrong ID.
	stageDir := t.TempDir()
	dstDB := filepath.Join(stageDir, "conversations", copied1.SessionID+".db")
	if err := os.MkdirAll(filepath.Dir(dstDB), 0o700); err != nil {
		t.Fatal(err)
	}
	rawDB, err := os.ReadFile(filepath.Join(copied1.Files[0].Root, filepath.FromSlash(copied1.Files[0].Path)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dstDB, rawDB, 0o600); err != nil {
		t.Fatal(err)
	}
	dstPlan := filepath.Join(stageDir, "brain", copied1.SessionID, "plan.md")
	if err := os.MkdirAll(filepath.Dir(dstPlan), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dstPlan, []byte("# Plan"), 0o600); err != nil {
		t.Fatal(err)
	}

	validated, err := ValidateStagedTransferFiles(context.Background(), stageDir, copied1.SessionID)
	if err != nil {
		t.Fatalf("ValidateStagedTransferFiles: %v", err)
	}
	if len(validated) != 2 {
		t.Fatalf("validated = %+v, want 2 files", validated)
	}

	// Mismatched cascade_id must fail validation.
	wrongID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := os.WriteFile(filepath.Join(stageDir, "conversations", wrongID+".db"), rawDB, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateStagedTransferFiles(context.Background(), stageDir, wrongID); err == nil {
		t.Fatal("ValidateStagedTransferFiles unexpectedly accepted mismatched cascade_id")
	}
}

func TestClassifyPermissionAutoAllowsAOMCPServers(t *testing.T) {
	t.Parallel()
	f := NewFlavor("")
	opts := []acp.PermissionOption{
		{OptionID: "allow", Name: "Allow", Kind: "allow_once"},
		{OptionID: "deny", Name: "Deny", Kind: "reject_once"},
	}

	for _, name := range []string{
		"mcp_ao-thread-tools_thread_options",
		"mcp__ao-thread-tools__thread_spawn",
		"mcp_ao-workflow-tools_run_list",
	} {
		cls := f.ClassifyPermission("t1", "turn1", "req1", acp.RequestPermissionParams{
			ToolCall: acp.ToolCallUpdate{ToolCallID: "c1", Name: name, Kind: "other"},
			Options:  opts,
		}, acp.PolicyPrompt)
		if cls.Action != acp.PermissionAutoAllow || cls.OptionID != "allow" {
			t.Fatalf("%s: got action=%v option=%q, want AutoAllow/allow", name, cls.Action, cls.OptionID)
		}
		// Read-only policy must NEVER be bypassed by AO MCP auto-approve.
		clsRO := f.ClassifyPermission("t1", "turn1", "req-ro", acp.RequestPermissionParams{
			ToolCall: acp.ToolCallUpdate{ToolCallID: "c-ro", Name: name, Kind: "other"},
			Options:  opts,
		}, acp.PolicyDenyMutating)
		if clsRO.Action != acp.PermissionAutoDeny {
			t.Fatalf("%s under PolicyDenyMutating: got action=%v, want AutoDeny", name, clsRO.Action)
		}
	}

	for _, name := range []string{
		"mcp_ao_thread_tools_evil_delete_repo",
		"mcp_ao-thread-tools_evil_tool",
		"mcp_ao-remote-tools_run_command",
		"mcp_github_create_issue",
		"run_command",
	} {
		cls := f.ClassifyPermission("t1", "turn1", "req2", acp.RequestPermissionParams{
			ToolCall: acp.ToolCallUpdate{ToolCallID: "c2", Name: name, Kind: "other"},
			Options:  opts,
		}, acp.PolicyPrompt)
		if cls.Action != acp.PermissionAskUser {
			t.Fatalf("%s: got action=%v, want PermissionAskUser", name, cls.Action)
		}
	}

	// A colliding third-party MCP server disables ambiguous single-underscore
	// auto-approval so a spoofed server can never bypass approval-required mode.
	fCollide := NewFlavor("")
	fCollide.BuildSessionMeta(acp.SessionConfig{
		MCPServers: []acp.MCPServerConfig{
			{Name: "ao-thread-tools"},
			{Name: "ao-thread-tools-thread"},
		},
	})
	clsCollide := fCollide.ClassifyPermission("t1", "turn1", "req-col", acp.RequestPermissionParams{
		ToolCall: acp.ToolCallUpdate{ToolCallID: "c-col", Name: "mcp_ao-thread-tools_thread_spawn", Kind: "other"},
		Options:  opts,
	}, acp.PolicyPrompt)
	if clsCollide.Action != acp.PermissionAskUser {
		t.Fatalf("colliding third-party server still auto-approved single-underscore tool: %v", clsCollide.Action)
	}

	// Global mcp_config.json with exact "ao_thread_tools", shorter prefix "ao",
	// or corrupt JSON must fail closed and disable auto-approval (both single-
	// and double-underscore forms).
	for _, tc := range []struct {
		name     string
		body     string
		toolName string
	}{
		{"exact_underscore_global", `{"mcpServers":{"ao_thread_tools":{"command":"evil"}}}`, "mcp_ao_thread_tools_thread_spawn"},
		{"exact_double_underscore_global", `{"mcpServers":{"ao-thread-tools":{"command":"evil"}}}`, "mcp__ao-thread-tools__thread_spawn"},
		{"shorter_prefix_global", `{"mcpServers":{"ao":{"command":"evil"}}}`, "mcp_ao_thread_tools_thread_spawn"},
		{"corrupt_global_config", `{not-valid-json`, "mcp_ao_thread_tools_thread_spawn"},
	} {
		homeDir := t.TempDir()
		appData := filepath.Join(homeDir, ".gemini", "antigravity-cli")
		cfgPath := filepath.Join(homeDir, ".gemini", "config", "mcp_config.json")
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		fGlobal := NewFlavor(appData)
		fGlobal.BuildSessionMeta(acp.SessionConfig{
			MCPServers: []acp.MCPServerConfig{{Name: "ao-thread-tools"}},
		})
		clsG := fGlobal.ClassifyPermission("t1", "turn1", "req-g", acp.RequestPermissionParams{
			ToolCall: acp.ToolCallUpdate{ToolCallID: "c-g", Name: tc.toolName, Kind: "other"},
			Options:  opts,
		}, acp.PolicyPrompt)
		if clsG.Action != acp.PermissionAskUser {
			t.Fatalf("%s: got action=%v, want PermissionAskUser", tc.name, clsG.Action)
		}
	}
}

func TestForkConversationEmptyCheckpointDeletesLaterSteps(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	srcID := "11111111-2222-3333-4444-555555555555"

	// Missing conversation DB before first write returns (-1, nil).
	missingCut, err := ReadForkCheckpoint(appDataDir, srcID)
	if err != nil || missingCut != -1 {
		t.Fatalf("ReadForkCheckpoint(missing) = (%d, %v), want (-1, nil)", missingCut, err)
	}

	dbPath := filepath.Join(appDataDir, "conversations", srcID+".db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE trajectory_meta (trajectory_id TEXT, cascade_id TEXT);
		INSERT INTO trajectory_meta VALUES ('t1', '` + srcID + `');
		CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, error_details BLOB, step_payload BLOB);
	`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	// Pre-cut checkpoint on empty table returns (-1, nil).
	cutIdx, err := ReadForkCheckpoint(appDataDir, srcID)
	if err != nil || cutIdx != -1 {
		_ = db.Close()
		t.Fatalf("ReadForkCheckpoint(empty) = (%d, %v), want (-1, nil)", cutIdx, err)
	}
	// Provider appends steps after the pre-cut checkpoint.
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, X'01'), (1, 15, X'02')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	forkID, cleanup, err := ForkConversation(appDataDir, srcID, cutIdx)
	if err != nil {
		t.Fatalf("ForkConversation(-1): %v", err)
	}
	defer func() { _ = cleanup() }()

	forkDB, err := sql.Open("sqlite", filepath.Join(appDataDir, "conversations", forkID+".db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = forkDB.Close() }()
	var count int
	if err := forkDB.QueryRow(`SELECT COUNT(*) FROM steps`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("empty-checkpoint fork retained %d later steps, want 0", count)
	}
}

func TestFileChangeDiffChangesFeedTriageExactPatch(t *testing.T) {
	t.Parallel()
	f := NewFlavor("")
	workDir := t.TempDir()
	helloPath := filepath.Join(workDir, "hello.txt")
	newPath := filepath.Join(workDir, "created.txt")
	delPath := filepath.Join(workDir, "removed.txt")

	encodeToolCompleteMeta := func(tc *acp.ToolCallUpdate) json.RawMessage {
		started := f.NormalizeToolStart(tc)
		comp := f.NormalizeToolComplete(tc, started)
		changes := make([]map[string]any, 0, len(comp.DiffChanges))
		for _, ch := range comp.DiffChanges {
			changes = append(changes, map[string]any{
				"path": ch.Path,
				"kind": map[string]any{"type": ch.Kind},
				"diff": ch.Diff,
			})
		}
		meta, err := json.Marshal(map[string]any{
			"toolName": comp.ToolName,
			"item": map[string]any{
				"id":      tc.ToolCallID,
				"type":    "file_change",
				"status":  "completed",
				"changes": changes,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return meta
	}

	// 1. Edit existing file -> exact_patch (+1 -1).
	editMeta := encodeToolCompleteMeta(&acp.ToolCallUpdate{
		ToolCallID: "call-edit",
		Name:       "edit_file",
		Kind:       "edit",
		Status:     "completed",
		Content: []acp.ToolContentItem{{
			Type:    "diff",
			Path:    helloPath,
			OldText: "line 1\nline 2\nline 3\n",
			NewText: "line 1\nline 2 edited\nline 3\n",
		}},
	})
	editRes, editDiff, ok := triage.ExtractFileChangeToolResult(editMeta, workDir)
	if !ok || editRes.InlineDiff == nil {
		t.Fatalf("edit ExtractFileChangeToolResult failed: ok=%v res=%+v", ok, editRes)
	}
	if editRes.InlineDiff.Availability != "exact_patch" || editRes.InlineDiff.Insertions != 1 || editRes.InlineDiff.Deletions != 1 || len(editDiff) == 0 {
		t.Fatalf("edit inline diff = %+v (bytes=%d), want exact_patch (+1 -1)", editRes.InlineDiff, len(editDiff))
	}

	// 2. Write new file -> exact_patch (+2 -0) with kind=added.
	writeMeta := encodeToolCompleteMeta(&acp.ToolCallUpdate{
		ToolCallID: "call-write",
		Name:       "write_file",
		Kind:       "edit",
		Status:     "completed",
		Content: []acp.ToolContentItem{{
			Type:    "diff",
			Path:    newPath,
			OldText: "",
			NewText: "alpha\nbeta\n",
		}},
	})
	writeRes, writeDiff, ok := triage.ExtractFileChangeToolResult(writeMeta, workDir)
	if !ok || writeRes.InlineDiff == nil {
		t.Fatalf("write ExtractFileChangeToolResult failed: ok=%v res=%+v", ok, writeRes)
	}
	if writeRes.InlineDiff.Availability != "exact_patch" || writeRes.InlineDiff.Insertions != 2 || writeRes.InlineDiff.Deletions != 0 || writeRes.InlineDiff.Files[0].Kind != "added" || len(writeDiff) == 0 {
		t.Fatalf("write inline diff = %+v (bytes=%d), want exact_patch added (+2 -0)", writeRes.InlineDiff, len(writeDiff))
	}

	// 3. Delete file -> exact_patch (+0 -2) with kind=deleted.
	delMeta := encodeToolCompleteMeta(&acp.ToolCallUpdate{
		ToolCallID: "call-del",
		Name:       "edit_file",
		Kind:       "edit",
		Status:     "completed",
		Content: []acp.ToolContentItem{{
			Type:    "diff",
			Path:    delPath,
			OldText: "old 1\nold 2\n",
			NewText: "",
		}},
	})
	delRes, delDiff, ok := triage.ExtractFileChangeToolResult(delMeta, workDir)
	if !ok || delRes.InlineDiff == nil {
		t.Fatalf("delete ExtractFileChangeToolResult failed: ok=%v res=%+v", ok, delRes)
	}
	if delRes.InlineDiff.Availability != "exact_patch" || delRes.InlineDiff.Insertions != 0 || delRes.InlineDiff.Deletions != 2 || delRes.InlineDiff.Files[0].Kind != "deleted" || len(delDiff) == 0 {
		t.Fatalf("delete inline diff = %+v (bytes=%d), want exact_patch deleted (+0 -2)", delRes.InlineDiff, len(delDiff))
	}
}

func TestParseMCPListOutput(t *testing.T) {
	t.Parallel()
	raw := `NAME    TYPE   STATUS    COMMAND/URL
demo    http   enabled   https://example.com/mcp
local   stdio  disabled  npx -y @example/mcp
broken  http   failed    https://broken.example/mcp
`
	statuses := ParseMCPListOutput(raw, time.Unix(1700000000, 0))
	if len(statuses) != 3 {
		t.Fatalf("len(statuses) = %d, want 3: %+v", len(statuses), statuses)
	}
	if statuses[0].Provider != mcpstatus.ProviderAgy || statuses[0].Name != "demo" || statuses[0].Status != mcpstatus.StatusNotStarted {
		t.Fatalf("statuses[0] = %+v", statuses[0])
	}
	if statuses[1].Name != "local" || statuses[1].Status != mcpstatus.StatusDisabled {
		t.Fatalf("statuses[1] = %+v", statuses[1])
	}
	if statuses[2].Name != "broken" || statuses[2].Status != mcpstatus.StatusFailed {
		t.Fatalf("statuses[2] = %+v", statuses[2])
	}
	if empty := ParseMCPListOutput("No MCP servers configured.\n", time.Now()); len(empty) != 0 {
		t.Fatalf("expected empty slice for no servers, got %+v", empty)
	}
}

func TestSQLiteFileDSNPreservesWindowsDriveAndEscapesDelimiters(t *testing.T) {
	t.Parallel()
	if got := sqliteFileDSN(`C:/Users/me/dir with space/a#1?b%2.db`, "mode=ro"); got != "file:C:/Users/me/dir with space/a%231%3Fb%252.db?mode=ro" {
		t.Fatalf("sqliteFileDSN = %q", got)
	}
}

func TestClassifyPermissionNormalizesToolNameAndInput(t *testing.T) {
	t.Parallel()
	f := NewFlavor("")
	cls := f.ClassifyPermission("t1", "turn-1", "req-1", acp.RequestPermissionParams{
		ToolCall: acp.ToolCallUpdate{
			ToolCallID: "call-1",
			Name:       "run_command",
			Title:      "Run echo",
			Kind:       "execute",
			RawInput:   json.RawMessage(`{"CommandLine":"echo hello","Cwd":"/repo"}`),
		},
		Options: []acp.PermissionOption{
			{OptionID: "allow", Name: "Allow", Kind: "allow_once"},
			{OptionID: "deny", Name: "Deny", Kind: "reject_once"},
		},
	}, acp.PolicyPrompt)
	if cls.Action != acp.PermissionAskUser {
		t.Fatalf("Action = %v, want PermissionAskUser", cls.Action)
	}
	if cls.Approval.ToolName != "Bash" {
		t.Fatalf("Approval.ToolName = %q, want Bash", cls.Approval.ToolName)
	}
	var in map[string]any
	if err := json.Unmarshal(cls.Approval.Input, &in); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if in["command"] != "echo hello" || in["cwd"] != "/repo" {
		t.Fatalf("Approval.Input = %s, want normalized command & cwd", string(cls.Approval.Input))
	}

	// File edit permission preserves ACP's pending diff preview alongside normalized path.
	editCls := f.ClassifyPermission("t1", "turn-1", "req-edit", acp.RequestPermissionParams{
		ToolCall: acp.ToolCallUpdate{
			ToolCallID: "call-edit",
			Name:       "replace_file_content",
			Title:      "Edit app.go",
			Kind:       "edit",
			RawInput:   json.RawMessage(`{"TargetFile":"/repo/app.go"}`),
			Content: []acp.ToolContentItem{{
				Type:    "diff",
				Path:    "/repo/app.go",
				OldText: "old line\n",
				NewText: "new line\n",
			}},
		},
		Options: []acp.PermissionOption{
			{OptionID: "allow", Name: "Allow", Kind: "allow_once"},
			{OptionID: "deny", Name: "Deny", Kind: "reject_once"},
		},
	}, acp.PolicyPrompt)
	var editIn map[string]any
	if err := json.Unmarshal(editCls.Approval.Input, &editIn); err != nil {
		t.Fatalf("unmarshal edit approval input: %v", err)
	}
	diffStr, _ := editIn["diff"].(string)
	if !strings.Contains(diffStr, "-old line") || !strings.Contains(diffStr, "+new line") {
		t.Fatalf("edit approval input missing unified diff preview: %s", string(editCls.Approval.Input))
	}
}

func TestCatalogCacheGetOrProbeCoalescesConcurrentMisses(t *testing.T) {
	t.Parallel()
	cache := NewCatalogCache()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls int
	var mu sync.Mutex
	probe := func(ctx context.Context, key string) ([]provider.ModelInfo, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(started)
			<-release
		}
		return []provider.ModelInfo{{Slug: "gemini-4-neon", Name: "Gemini 4 Neon", Provider: string(provider.Agy)}}, nil
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	errs := make([]error, 4)
	results := make([][]provider.ModelInfo, 4)
	for i := range 4 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx := context.Background()
			if idx == 0 {
				ctx = cancelCtx
			}
			results[idx], errs[idx] = cache.GetOrProbe(ctx, "agy", probe)
		}(i)
	}
	<-started
	// Cancelling one waiter must return context.Canceled without aborting siblings.
	cancel()
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("probe called %d times for 4 concurrent misses, want 1", gotCalls)
	}
	if !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("cancelled waiter err = %v, want context.Canceled", errs[0])
	}
	for i := 1; i < 4; i++ {
		if errs[i] != nil || len(results[i]) != 1 || results[i][0].Slug != "gemini-4-neon" {
			t.Fatalf("waiter %d = (%+v, %v), want 1 model", i, results[i], errs[i])
		}
	}

	// Clear() mid-flight must prevent a post-Clear caller from joining the stale
	// flight and receiving pre-invalidation results.
	staleStarted := make(chan struct{})
	staleRelease := make(chan struct{})
	var flightSeq int
	staleProbe := func(ctx context.Context, key string) ([]provider.ModelInfo, error) {
		mu.Lock()
		flightSeq++
		seq := flightSeq
		mu.Unlock()
		if seq == 1 {
			close(staleStarted)
			<-staleRelease
			return []provider.ModelInfo{{Slug: "old-model", Provider: string(provider.Agy)}}, nil
		}
		return []provider.ModelInfo{{Slug: "fresh-model", Provider: string(provider.Agy)}}, nil
	}
	cache.Clear()
	staleDone := make(chan struct{})
	go func() {
		defer close(staleDone)
		_, _ = cache.GetOrProbe(context.Background(), "agy", staleProbe)
	}()
	<-staleStarted
	cache.Clear()
	freshModels, freshErr := cache.GetOrProbe(context.Background(), "agy", staleProbe)
	close(staleRelease)
	<-staleDone
	if freshErr != nil || len(freshModels) != 1 || freshModels[0].Slug != "fresh-model" {
		t.Fatalf("post-Clear caller got (%+v, %v), want fresh-model", freshModels, freshErr)
	}
}
