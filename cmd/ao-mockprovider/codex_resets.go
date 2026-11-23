package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/provider/codex/codexfake"
)

// Earned rate-limit resets, answered from the harness-seeded state in
// codexfake (under the home directory, which the app's account probes
// inherit without any harness environment). Shapes are codex-rs
// app-server's (README §7 and §8): the rate-limit read carries
// `rateLimitResetCredits`, null when nothing is set, and
// `account/rateLimitResetCredit/consume` answers `{outcome}` or the
// app-server's own -32600 for an empty key.

// mockResetStore is the reset state of this mock install. A process with no
// home answers as an account with no resets.
func mockResetStore() (codexfake.Store, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return codexfake.Store{}, false
	}
	return codexfake.Store{Home: home}, true
}

// mockCodexAccountID is the account the credential in CODEX_HOME names
// (`tokens.account_id`, which the mock's own sign-in writes), "" when there
// is none to read.
func mockCodexAccountID() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return ""
	}
	var auth struct {
		Tokens struct {
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &auth) != nil {
		return ""
	}
	return strings.TrimSpace(auth.Tokens.AccountID)
}

// mockRateLimits is the `rateLimits` value, JSON-encoded: the plan, and the
// 5-hour window the harness seeded for the account, if any.
func mockRateLimits() string {
	limits := map[string]any{"limitId": "codex", "planType": "pro"}
	store, ok := mockResetStore()
	accountID := mockCodexAccountID()
	if ok && accountID != "" {
		used, err := store.FiveHourWindow(accountID)
		if err != nil {
			log.Printf("codex: usage for %s: %v; answering no window", accountID, err)
		} else if used != nil {
			limits["primary"] = map[string]any{
				"usedPercent":        *used,
				"windowDurationMins": 300,
				"resetsAt":           time.Now().Add(5 * time.Hour).Unix(),
			}
		}
	}
	return mustJSON(limits)
}

// mockResetSummary is the `rateLimitResetCredits` value, JSON-encoded.
func mockResetSummary() string {
	store, ok := mockResetStore()
	accountID := mockCodexAccountID()
	if !ok || accountID == "" {
		return "null"
	}
	summary, err := store.Summary(accountID)
	if err != nil {
		log.Printf("codex: reset credits for %s: %v; answering none", accountID, err)
		return "null"
	}
	return mustJSON(summary)
}

// handleResetRequest answers the consume method. Returns false for any other
// method.
func (a *codexAdapter) handleResetRequest(id json.RawMessage, method string, params json.RawMessage) bool {
	if method != "account/rateLimitResetCredit/consume" {
		return false
	}
	key := readParamString(params, "idempotencyKey")
	if key == "" {
		a.writeRPCError(id, -32600, "idempotencyKey must not be empty")
		return true
	}
	// A consume that names no credit is the fake's error (codexfake.Consume).
	creditID := readParamString(params, "creditId")
	store, ok := mockResetStore()
	accountID := mockCodexAccountID()
	if !ok || accountID == "" {
		a.writeRPCError(id, -32600, "codex account authentication required for rate limit reset credits")
		return true
	}
	outcome, lost, err := store.Consume(accountID, key, creditID)
	if err != nil {
		a.writeRPCError(id, -32603, fmt.Sprintf("failed to consume rate limit reset: %v", err))
		return true
	}
	if lost {
		a.writeRPCError(id, -32603, "rate limit reset consume timed out")
		return true
	}
	a.w.writeLine(mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  map[string]any{"outcome": outcome},
	}), 0, 0)
	return true
}
