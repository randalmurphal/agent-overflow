package agy

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
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
	defer db.Close()

	stmts := []string{
		`CREATE TABLE trajectory_meta (trajectory_id TEXT, cascade_id TEXT)`,
		`CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, data BLOB)`,
		`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB)`,
		`CREATE TABLE executor_metadata (idx INTEGER PRIMARY KEY, data BLOB)`,
		`INSERT INTO trajectory_meta (trajectory_id, cascade_id) VALUES ('orig-traj', '` + sessionID + `')`,
		`INSERT INTO steps (idx, step_type, data) VALUES (0, 1, 'turn 0 user')`,
		`INSERT INTO steps (idx, step_type, data) VALUES (1, 2, 'turn 0 reply')`,
		`INSERT INTO steps (idx, step_type, data) VALUES (2, 1, 'turn 1 user')`,
		`INSERT INTO steps (idx, step_type, data) VALUES (3, 2, 'turn 1 reply')`,
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
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, data) VALUES (4, 17, ?)`, []byte("\x0a\x1eQUOTA_EXHAUSTED: try again later")); err != nil {
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

	dstDB, err := sql.Open("sqlite", conversationDBPath(appDataDir, dstID))
	if err != nil {
		t.Fatal(err)
	}
	defer dstDB.Close()

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
	badPath := conversationDBPath(appDataDir, badID)
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
import json, sys

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

model = "gemini-4-neon"
matrix = {
    "gemini-4-neon": (1048576, "high", [("low","Low"),("medium","Medium"),("high","High"),("xhigh","xHigh")]),
    "gpt-oss-120b-medium": (131072, "", []),
}

def opts(m):
    _, cur_t, ts = matrix[m]
    out = [{
        "id": "model", "name": "Model", "type": "select", "currentValue": m,
        "options": [
            {"value": "gemini-4-neon", "name": "Gemini 4 Neon"},
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
        send({"jsonrpc":"2.0","id":mid,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"Fake Agy","version":"1.5.13"}}})
    elif m == "session/new":
        send({"jsonrpc":"2.0","id":mid,"result":{"sessionId":"probe-1","configOptions":opts(model)}})
        send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"probe-1","update":{"sessionUpdate":"usage_update","used":0,"size":matrix[model][0]}}})
    elif m == "session/set_config_option":
        model = p["value"]
        send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"probe-1","update":{"sessionUpdate":"usage_update","used":0,"size":matrix[model][0]}}})
        send({"jsonrpc":"2.0","id":mid,"result":{"configOptions":opts(model)}})
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
	if len(models) != 2 {
		t.Fatalf("len(models) = %d, want 2: %+v", len(models), models)
	}
	if models[0].Slug != "gemini-4-neon" || !models[0].IsDefault || len(models[0].ReasoningEfforts) != 4 || models[0].ContextWindows[0].Tokens != 1048576 {
		t.Fatalf("unexpected model 0: %+v", models[0])
	}
	if models[1].Slug != "gpt-oss-120b-medium" || len(models[1].ReasoningEfforts) != 0 || models[1].ContextWindows[0].Tokens != 131072 {
		t.Fatalf("unexpected model 1: %+v", models[1])
	}
}
