package claude

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// fixtureCategories is the verbatim `categories[]` array from
// docs/references/fixtures/claude/context_usage_control_20260803.summary.json
// (Claude Code 2.1.219). Keeping it inline as the wire JSON — rather than a
// hand-built Go literal — means the test exercises the real decode path,
// including the keys we deliberately drop (`color`) and the optional one we
// keep (`isDeferred`).
const fixtureCategories = `[
  {"name":"System prompt","tokens":4027,"color":"promptBorder"},
  {"name":"System tools","tokens":15397,"color":"inactive"},
  {"name":"System tools (deferred)","tokens":13467,"color":"inactive","isDeferred":true},
  {"name":"Custom agents","tokens":105,"color":"permission"},
  {"name":"Memory files","tokens":1424,"color":"claude"},
  {"name":"Skills","tokens":3067,"color":"warning"},
  {"name":"Messages","tokens":8,"color":"purple_FOR_SUBAGENTS_ONLY"},
  {"name":"Autocompact buffer","tokens":33000,"color":"inactive"},
  {"name":"Free space","tokens":942972,"color":"promptBorder"}
]`

// fixturePayload is the full control_response `response.response` object,
// trimmed to the keys the parser reads plus a representative sample of the
// ones it must ignore without complaint.
const fixturePayload = `{
  "categories": ` + fixtureCategories + `,
  "totalTokens": 24028,
  "maxTokens": 1000000,
  "rawMaxTokens": 1000000,
  "autocompactSource": "env",
  "percentage": 2,
  "gridRows": [[{"color":"promptBorder","isFilled":true,"categoryName":"System prompt","tokens":4027,"percentage":0,"squareFullness":0.8054}]],
  "model": "claude-fable-5",
  "memoryFiles": [{"path":"/home/u/.claude/CLAUDE.md","type":"User","tokens":1424}],
  "mcpTools": [],
  "agents": [{"agentType":"Spike","source":"userSettings","tokens":53}],
  "slashCommands": {"totalCommands":40,"includedCommands":40,"tokens":900},
  "skills": {"totalSkills":23,"includedSkills":23,"tokens":3067,"skillFrontmatter":[]},
  "autoCompactThreshold": 967000,
  "isAutoCompactEnabled": true,
  "messageBreakdown": {"toolCallTokens":0,"toolResultTokens":0,"attachmentTokens":0,"assistantMessageTokens":0,"userMessageTokens":8,"redirectedContextTokens":0,"unattributedTokens":0,"toolCallsByType":[],"attachmentsByType":[]},
  "apiUsage": null
}`

func TestParseContextUsage_Fixture(t *testing.T) {
	usage, err := ParseContextUsage(json.RawMessage(fixturePayload))
	if err != nil {
		t.Fatalf("ParseContextUsage: %v", err)
	}
	if usage.TotalTokens != 24028 {
		t.Errorf("TotalTokens = %d, want 24028", usage.TotalTokens)
	}
	if usage.MaxTokens != 1_000_000 {
		t.Errorf("MaxTokens = %d, want 1000000", usage.MaxTokens)
	}
	if usage.Percentage != 2 {
		t.Errorf("Percentage = %d, want 2", usage.Percentage)
	}
	if usage.Model != "claude-fable-5" {
		t.Errorf("Model = %q, want claude-fable-5", usage.Model)
	}
	if len(usage.Categories) != 9 {
		t.Fatalf("Categories = %d rows, want 9: %+v", len(usage.Categories), usage.Categories)
	}
	// Wire order is preserved — the CLI orders the breakdown deliberately
	// (Free space last), and the UI renders it in that order.
	if usage.Categories[0].Name != "System prompt" {
		t.Errorf("first category = %q, want System prompt", usage.Categories[0].Name)
	}
	if last := usage.Categories[len(usage.Categories)-1]; last.Name != "Free space" || last.Tokens != 942972 {
		t.Errorf("last category = %+v, want Free space/942972", last)
	}
	if !usage.Categories[2].Deferred {
		t.Errorf("category %q should carry Deferred=true", usage.Categories[2].Name)
	}
	for i, cat := range usage.Categories {
		if i == 2 {
			continue
		}
		if cat.Deferred {
			t.Errorf("category %q must not be marked deferred", cat.Name)
		}
	}
}

// The wire's own arithmetic is a contract the UI leans on: summing every row
// overcounts by the deferred total, and the non-deferred rows tile the whole
// window. Assert it against the fixture so a parser change that (say) folded
// deferred rows into their parent category would be caught here.
func TestParseContextUsage_DeferredIsExcludedFromTotal(t *testing.T) {
	usage, err := ParseContextUsage(json.RawMessage(fixturePayload))
	if err != nil {
		t.Fatalf("ParseContextUsage: %v", err)
	}
	var counted, deferred, nonDeferred int
	for _, cat := range usage.Categories {
		if cat.Deferred {
			deferred += cat.Tokens
			continue
		}
		nonDeferred += cat.Tokens
		if cat.Name != "Free space" && cat.Name != "Autocompact buffer" {
			counted += cat.Tokens
		}
	}
	if counted != usage.TotalTokens {
		t.Errorf("non-deferred, non-slack rows sum to %d, want TotalTokens %d", counted, usage.TotalTokens)
	}
	if deferred == 0 {
		t.Fatal("fixture should contain a deferred row")
	}
	if nonDeferred != usage.MaxTokens {
		t.Errorf("non-deferred rows sum to %d, want MaxTokens %d", nonDeferred, usage.MaxTokens)
	}
}

// Category names are not an enum on our side. A release that renames a
// category or adds a new one must flow straight through to the UI rather
// than being dropped on the floor.
func TestParseContextUsage_UnknownCategoriesPassThrough(t *testing.T) {
	payload := `{"totalTokens":50,"maxTokens":1000,"rawMaxTokens":1000,"percentage":5,"model":"m",
	  "categories":[{"name":"Quantum ledger","tokens":30,"color":"nope"},
	                {"name":"Something else entirely","tokens":20,"isDeferred":true}]}`
	usage, err := ParseContextUsage(json.RawMessage(payload))
	if err != nil {
		t.Fatalf("ParseContextUsage: %v", err)
	}
	if len(usage.Categories) != 2 {
		t.Fatalf("Categories = %+v, want both unknown rows retained", usage.Categories)
	}
	if usage.Categories[0].Name != "Quantum ledger" || usage.Categories[0].Tokens != 30 {
		t.Errorf("first row = %+v", usage.Categories[0])
	}
	if !usage.Categories[1].Deferred {
		t.Errorf("second row should keep isDeferred: %+v", usage.Categories[1])
	}
}

// A response with no categories at all is still a valid answer (a session
// the CLI reports as empty) — it must not be mistaken for a failure.
func TestParseContextUsage_NoCategoriesIsNotAnError(t *testing.T) {
	usage, err := ParseContextUsage(json.RawMessage(`{"totalTokens":0,"maxTokens":200000,"rawMaxTokens":200000,"percentage":0,"model":"m"}`))
	if err != nil {
		t.Fatalf("ParseContextUsage: %v", err)
	}
	if usage.Categories == nil {
		t.Error("Categories should be an empty slice, not nil — the UI iterates it directly")
	}
	if len(usage.Categories) != 0 {
		t.Errorf("Categories = %+v, want empty", usage.Categories)
	}
}

// maxTokens and rawMaxTokens have been identical on every capture, but the
// CLI declares both. Either alone must produce a usable window.
func TestParseContextUsage_FallsBackToRawMaxTokens(t *testing.T) {
	usage, err := ParseContextUsage(json.RawMessage(`{"totalTokens":10,"rawMaxTokens":200000,"percentage":0,"categories":[]}`))
	if err != nil {
		t.Fatalf("ParseContextUsage: %v", err)
	}
	if usage.MaxTokens != 200000 {
		t.Errorf("MaxTokens = %d, want the rawMaxTokens fallback 200000", usage.MaxTokens)
	}
}

// Without a window there is no occupancy to render, and a zero denominator
// downstream would read as "0% used" on a full context. Fail loudly.
func TestParseContextUsage_NoWindowIsAnError(t *testing.T) {
	_, err := ParseContextUsage(json.RawMessage(`{"totalTokens":1234,"percentage":40,"categories":[]}`))
	if err == nil {
		t.Fatal("expected an error when neither maxTokens nor rawMaxTokens is present")
	}
	if !strings.Contains(err.Error(), "no context window") {
		t.Errorf("error should name the missing window, got: %v", err)
	}
}

func TestParseContextUsage_EmptyPayload(t *testing.T) {
	if _, err := ParseContextUsage(nil); err == nil {
		t.Fatal("expected an error on an empty payload")
	}
}

func TestParseContextUsage_MalformedPayload(t *testing.T) {
	_, err := ParseContextUsage(json.RawMessage(`{"categories":"oops","maxTokens":1000}`))
	if err == nil {
		t.Fatal("expected a decode error when categories is not an array")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("error should wrap %q, got %v", "decode response", err)
	}
}

// contextUsageResponderScript writes a fake Claude CLI that answers
// get_context_usage control_requests. Mirrors mcpStatusResponderScript so
// the round-trip exercises the real proc + readLoop + sendControlRequest
// machinery rather than mocking session internals.
func contextUsageResponderScript(mode string) string {
	const header = `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"get_context_usage"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
`
	const footer = `
            ;;
    esac
done
`
	var body string
	switch mode {
	case "success":
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"totalTokens":24028,"maxTokens":1000000,"rawMaxTokens":1000000,"percentage":2,"model":"claude-fable-5","categories":[{"name":"System prompt","tokens":4027,"color":"promptBorder"},{"name":"Free space","tokens":975972,"color":"promptBorder"}]}}}\n' "$reqid"`
	case "error-subtype":
		body = `            printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"context analysis failed"}}\n' "$reqid"`
	case "success-no-payload":
		// A success with no body at all: the CLI's generic
		// sendControlResponseSuccess path can emit this. There is no
		// honest breakdown to show, so it must surface as an error
		// rather than as an all-zero chart.
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s"}}\n' "$reqid"`
	case "silent":
		body = `            : # never respond — exercises the timeout path`
	default:
		body = `            : # unknown mode — never happens in tests`
	}
	return header + body + footer
}

func newContextUsageResponderSession(t *testing.T, mode string, timeout time.Duration) *Session {
	t.Helper()
	scriptPath := t.TempDir() + "/fake-claude"
	if err := os.WriteFile(scriptPath, []byte(contextUsageResponderScript(mode)), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: scriptPath})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:                  proc,
		threadID:              "thread-context-usage-test",
		onEvent:               func(evt provider.ProviderEvent) { _ = evt },
		cancel:                cancel,
		readDone:              make(chan struct{}),
		controlRequestTimeout: timeout,
	}
	go s.readLoop()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSession_GetContextUsage_RoundTrip(t *testing.T) {
	s := newContextUsageResponderSession(t, "success", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	usage, err := s.GetContextUsage(ctx)
	if err != nil {
		t.Fatalf("GetContextUsage: %v", err)
	}
	if usage.TotalTokens != 24028 || usage.MaxTokens != 1_000_000 {
		t.Errorf("usage = %+v, want 24028/1000000", usage)
	}
	if len(usage.Categories) != 2 {
		t.Errorf("Categories = %+v, want 2 rows", usage.Categories)
	}
}

func TestSession_GetContextUsage_ErrorSubtype(t *testing.T) {
	s := newContextUsageResponderSession(t, "error-subtype", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := s.GetContextUsage(ctx); err == nil {
		t.Fatal("expected an error, got nil")
	} else if !strings.Contains(err.Error(), "context analysis failed") {
		t.Errorf("error should carry the CLI message, got: %v", err)
	}
}

func TestSession_GetContextUsage_SuccessWithNoPayload(t *testing.T) {
	s := newContextUsageResponderSession(t, "success-no-payload", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := s.GetContextUsage(ctx); err == nil {
		t.Fatal("expected an error on a bodyless success, got nil")
	}
}

// A wedged CLI must surface as a timeout error and leave the session alive —
// the same contract every other outbound control_request holds to.
func TestSession_GetContextUsage_TimeoutDoesNotKillSession(t *testing.T) {
	s := newContextUsageResponderSession(t, "silent", 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := s.GetContextUsage(ctx); err == nil {
		t.Fatal("expected a timeout error, got nil")
	} else if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error should mention the timeout, got: %v", err)
	}
	if s.closing.Load() {
		t.Error("a control-request timeout must not close the session")
	}
	// And a second call still reaches the (still-running) process.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if _, err := s.GetContextUsage(ctx2); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("second call should time out against the live session, got: %v", err)
	}
}
