package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// The provider-estimate feature is an OVERLAY on a complete accounting path
// that already works without it. These tests drive a real Codex session
// through the app (mock app-server, no network, no real binary) and pin the
// two halves of that promise: when `account/usage/read` answers, the estimate
// lands and re-labels the thread's lifetime bucket; when it fails, the turn
// still settles and the rate-table figure is still what the UI shows.

const threadCostProviderThread = "codex-thread-cost"

// writeThreadCostCodex mocks an app-server that reports a 0.149 build at
// initialize (the only place a live process states its own version — see
// app_server_version.go), streams a complete turn with a cumulative token
// total, and answers `account/usage/read` with whatever the caller wants.
//
// Every `account/usage/read` request line is appended to capturePath, which
// is what makes "the read was attempted and failed" observable rather than
// raced against.
func writeThreadCostCodex(t *testing.T, usageReply, capturePath string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
turn=0
while IFS= read -r line; do
  id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
  if [ -z "$id" ]; then continue; fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"userAgent":"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) app_server"}}\n' "$id"
    continue
  fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
    continue
  fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"account/usage/read"'; then
    printf '%%s\n' "$line" >> %q
    printf %q "$id"
    continue
  fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/start"'; then
    turn=$((turn+1))
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"turn":{"id":"turn-%%s"}}}\n' "$id" "$turn"
    printf '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"%s","turn":{"id":"turn-%%s"}}}\n' "$turn"
    printf '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"%s","tokenUsage":{"last":{"totalTokens":1500},"total":{"inputTokens":1000,"cachedInputTokens":200,"outputTokens":500,"totalTokens":1500},"modelContextWindow":272000}}}\n'
    printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"%s","turnId":"turn-%%s","item":{"id":"msg-%%s","type":"agentMessage","text":"done"}}}\n' "$turn" "$turn"
    printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"%s","turn":{"id":"turn-%%s","status":"completed"}}}\n' "$turn"
    continue
  fi
  printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`,
		threadCostProviderThread,
		capturePath,
		usageReply+"\n",
		threadCostProviderThread,
		threadCostProviderThread,
		threadCostProviderThread,
		threadCostProviderThread,
	)
	path := filepath.Join(t.TempDir(), "codex-thread-cost.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write mock codex binary: %v", err)
	}
	return path
}

// startThreadCostSession seeds a Codex thread on the mock binary and runs one
// full turn through it, returning the thread id.
func startThreadCostSession(t *testing.T, app *App, id, usageReply, capturePath string) string {
	t.Helper()
	thread := testThread(id)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	// Production builds the router in initSubsystems, before any session can
	// start; this fixture builds an App directly and would otherwise leave it
	// nil until the first send. The session's own `EventInit` — which is what
	// writes `threads.session_ref`, and therefore what a stored provider cost
	// is checked against — arrives before that, so without this the thread
	// would run its whole turn still pointing at no provider thread.
	app.ensureTriageRouter()
	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath": writeThreadCostCodex(t, usageReply, capturePath),
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if err := app.StartSession(id); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	t.Cleanup(func() { _ = app.StopSession(id) })
	if err := app.SendMessage(id, "price this", nil); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	return id
}

// waitForSettledTurn blocks until the thread has one turn row with a
// completed_at — the "the turn persisted" signal every caller here needs.
func waitForSettledTurn(t *testing.T, app *App, threadID string) store.Turn {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		turns, err := app.store.ListRecentTurns(threadID, 5)
		if err == nil && len(turns) > 0 && turns[0].CompletedAt != nil {
			return turns[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	turns, err := app.store.ListRecentTurns(threadID, 5)
	t.Fatalf("no settled turn on %s: turns=%+v err=%v", threadID, turns, err)
	return store.Turn{}
}

// waitForUsageReadAttempt blocks until the mock recorded an
// `account/usage/read` request line.
func waitForUsageReadAttempt(t *testing.T, capturePath string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(capturePath)
		if err == nil && strings.TrimSpace(string(body)) != "" {
			return string(body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no account/usage/read request reached the app-server (capture %s)", capturePath)
	return ""
}

func threadLifetimeBucket(t *testing.T, app *App, threadID string) store.UsageBucket {
	t.Helper()
	buckets, err := app.GetUsageStats(store.UsageQuery{ThreadID: threadID})
	if err != nil {
		t.Fatalf("GetUsageStats() error = %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("GetUsageStats() returned %d buckets, want 1: %+v", len(buckets), buckets)
	}
	return buckets[0]
}

// TestCodexTurnPersistsWhenTheThreadCostReadFails is the load-bearing
// failure-path test: the estimate read is a side effect of a turn that has
// ALREADY been persisted, so an app-server that refuses it must cost the user
// nothing. The turn settles, the ledger row is written, no provider-estimate
// row appears, and the usage chip still reads the rate-table figure with no
// source label.
func TestCodexTurnPersistsWhenTheThreadCostReadFails(t *testing.T) {
	app := newTestAppWithStore(t)
	capturePath := filepath.Join(t.TempDir(), "usage-read.jsonl")
	threadID := startThreadCostSession(t, app, "codex-cost-fails",
		`{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"internal error"}}`, capturePath)

	turn := waitForSettledTurn(t, app, threadID)
	if turn.StopReason == "interrupted" {
		t.Fatalf("turn was interrupted rather than completed: %+v", turn)
	}
	waitForUsageReadAttempt(t, capturePath)

	if _, found, err := app.store.GetProviderThreadCost(threadID); err != nil || found {
		t.Fatalf("a failed read wrote a provider cost row (found %v, err %v)", found, err)
	}

	bucket := threadLifetimeBucket(t, app, threadID)
	if bucket.CostSource != "" {
		t.Fatalf("bucket costSource = %q, want \"\" (rate-table fallback)", bucket.CostSource)
	}
	if bucket.OutputTokens == 0 {
		t.Fatalf("the ledger row did not persist: %+v", bucket)
	}
	if bucket.CostUSD <= 0 {
		t.Fatalf("bucket CostUSD = %v, want the rate-table price: %+v", bucket.CostUSD, bucket)
	}
}

// TestCodexThreadCostReplacesTheThreadLifetimeCost is the success path: the
// provider figure is persisted at the provider's own grain (one cumulative
// row per thread), the token counts are untouched, and only the ONE query
// shape a cumulative total can answer is re-priced and labelled.
func TestCodexThreadCostReplacesTheThreadLifetimeCost(t *testing.T) {
	app := newTestAppWithStore(t)
	capturePath := filepath.Join(t.TempDir(), "usage-read.jsonl")
	threadID := startThreadCostSession(t, app, "codex-cost-lands",
		`{"jsonrpc":"2.0","id":%s,"result":{"threadUsage":{"threadId":"`+threadCostProviderThread+
			`","estimatedUsageCreditsMicros":4200000,"estimatedUsageUsdMicros":137500,"groups":[]}}}`,
		capturePath)

	waitForSettledTurn(t, app, threadID)
	request := waitForUsageReadAttempt(t, capturePath)
	if !strings.Contains(request, `"threadId":"`+threadCostProviderThread+`"`) {
		t.Fatalf("account/usage/read did not carry the thread id: %s", request)
	}

	deadline := time.Now().Add(10 * time.Second)
	var cost store.ProviderThreadCost
	for time.Now().Before(deadline) {
		got, found, err := app.store.GetProviderThreadCost(threadID)
		if err != nil {
			t.Fatalf("GetProviderThreadCost() error = %v", err)
		}
		if found {
			cost = got
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cost.CostUSDMicros != 137500 {
		t.Fatalf("provider cost = %+v, want 137500 usd micros", cost)
	}
	// The row names the Codex thread the backend priced, which is the same
	// thread `threads.session_ref` points at — that equality is what makes the
	// row readable at all (migration v68).
	if cost.SessionRef != threadCostProviderThread {
		t.Fatalf("cost sessionRef = %q, want the Codex thread the estimate describes (%q)",
			cost.SessionRef, threadCostProviderThread)
	}
	if cost.CostSource != store.ProviderThreadCostSourceEstimate {
		t.Fatalf("cost source = %q, want %q", cost.CostSource, store.ProviderThreadCostSourceEstimate)
	}
	if cost.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", cost.Provider)
	}

	bucket := threadLifetimeBucket(t, app, threadID)
	if bucket.CostUSD != 0.1375 {
		t.Fatalf("lifetime bucket CostUSD = %v, want 0.1375 (the provider's own figure)", bucket.CostUSD)
	}
	if bucket.CostSource != store.ProviderThreadCostSourceEstimate {
		t.Fatalf("lifetime bucket costSource = %q, want %q", bucket.CostSource, store.ProviderThreadCostSourceEstimate)
	}
	if bucket.OutputTokens != 500 || bucket.InputTokens != 800 || bucket.CacheReadInputTokens != 200 {
		t.Fatalf("the estimate disturbed the token counts: %+v", bucket)
	}

	// The estimate answers ONE question. A grouped query over the same
	// thread stays rate-table priced and unlabelled, because the provider
	// states a single number for the thread's whole life and nothing can
	// split it per model.
	grouped, err := app.GetUsageStats(store.UsageQuery{ThreadID: threadID, GroupBy: "model"})
	if err != nil {
		t.Fatalf("GetUsageStats(model) error = %v", err)
	}
	for _, b := range grouped {
		if b.CostSource != "" {
			t.Fatalf("grouped bucket %q carried costSource %q", b.Bucket, b.CostSource)
		}
		if b.CostUSD == 0.1375 {
			t.Fatalf("grouped bucket %q was re-priced from the thread estimate", b.Bucket)
		}
	}
}

// TestProviderThreadCostOverlayIsScopedToTheLifetimeThreadQuery pins the
// narrow shape the overlay claims. Every other query either splits the
// estimate across a dimension the provider never stated (time, model) or
// mixes it into a total that also contains rate-table figures, so the whole
// answer stays rate-table priced rather than partly re-based.
func TestProviderThreadCostOverlayIsScopedToTheLifetimeThreadQuery(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("codex-overlay-scope")
	thread.SessionRef = "codex-overlay-provider-thread"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.AppendUsage([]store.UsageLedgerRow{{
		CreatedAt: now, ProjectID: defaultTestProjectID, ThreadID: thread.ID, TurnID: "t1",
		Provider: "codex", Model: "gpt-5.4", InputTokens: 1000, OutputTokens: 500,
	}}); err != nil {
		t.Fatalf("AppendUsage() error = %v", err)
	}
	tableCost := threadLifetimeBucket(t, app, thread.ID).CostUSD
	if tableCost <= 0 {
		t.Fatalf("rate-table baseline = %v, want > 0", tableCost)
	}

	if err := app.store.PutProviderThreadCost(store.ProviderThreadCost{
		ThreadID: thread.ID, SessionRef: thread.SessionRef,
		Provider: "codex", CostUSDMicros: 137500, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("PutProviderThreadCost() error = %v", err)
	}
	if got := threadLifetimeBucket(t, app, thread.ID); got.CostUSD != 0.1375 ||
		got.CostSource != store.ProviderThreadCostSourceEstimate {
		t.Fatalf("lifetime bucket = %+v, want the provider estimate", got)
	}

	// Each of these must keep the rate-table answer.
	for name, query := range map[string]store.UsageQuery{
		"day buckets":       {ThreadID: thread.ID, GroupBy: "day"},
		"model buckets":     {ThreadID: thread.ID, GroupBy: "model"},
		"time bounded":      {ThreadID: thread.ID, FromMillis: now - 1000},
		"model filtered":    {ThreadID: thread.ID, Model: "gpt-5.4"},
		"other provider":    {ThreadID: thread.ID, Provider: "claude"},
		"no thread at all":  {},
		"thread dimensions": {GroupBy: "thread"},
	} {
		buckets, err := app.GetUsageStats(query)
		if err != nil {
			t.Fatalf("GetUsageStats(%s) error = %v", name, err)
		}
		for _, b := range buckets {
			if b.CostSource != "" {
				t.Fatalf("%s: bucket %q carried costSource %q", name, b.Bucket, b.CostSource)
			}
			if b.CostUSD == 0.1375 {
				t.Fatalf("%s: bucket %q was re-priced from the thread estimate", name, b.Bucket)
			}
		}
	}
}
