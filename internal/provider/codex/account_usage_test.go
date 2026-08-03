package codex

import (
	"encoding/json"
	"errors"
	"testing"
)

// accountUsageFixture is the shape codex-cli 0.146.0 returned for a real
// ChatGPT account (scratchpad spike, 2026-08-02), trimmed to three buckets.
const accountUsageFixture = `{
  "summary": {
    "lifetimeTokens": 11776335004,
    "peakDailyTokens": 821866242,
    "longestRunningTurnSec": 19486,
    "currentStreakDays": 8,
    "longestStreakDays": 17
  },
  "dailyUsageBuckets": [
    {"startDate": "2026-07-31", "tokens": 11585916},
    {"startDate": "2026-08-01", "tokens": 725458670},
    {"startDate": "2026-08-02", "tokens": 1174183}
  ]
}`

func TestParseAccountUsage(t *testing.T) {
	usage, err := parseAccountUsage(json.RawMessage(accountUsageFixture))
	if err != nil {
		t.Fatalf("parseAccountUsage: %v", err)
	}
	if usage.LifetimeTokens == nil || *usage.LifetimeTokens != 11776335004 {
		t.Fatalf("lifetime tokens = %v, want 11776335004", usage.LifetimeTokens)
	}
	if usage.LongestRunningTurnSec == nil || *usage.LongestRunningTurnSec != 19486 {
		t.Errorf("longest turn = %v, want 19486", usage.LongestRunningTurnSec)
	}
	if len(usage.DailyBuckets) != 3 {
		t.Fatalf("buckets = %d, want 3", len(usage.DailyBuckets))
	}
	if usage.DailyBuckets[1] != (AccountUsageDailyBucket{StartDate: "2026-08-01", Tokens: 725458670}) {
		t.Errorf("second bucket = %+v", usage.DailyBuckets[1])
	}
	if usage.Empty() {
		t.Error("a populated report must not read as empty")
	}
}

// TestParseAccountUsageAbsenceIsNotZero pins the one rule the whole surface
// rests on: a field the backend omitted stays nil so the UI can render
// nothing, instead of claiming the account has used zero tokens.
func TestParseAccountUsageAbsenceIsNotZero(t *testing.T) {
	usage, err := parseAccountUsage(json.RawMessage(`{"summary":{"lifetimeTokens":0},"dailyUsageBuckets":null}`))
	if err != nil {
		t.Fatalf("parseAccountUsage: %v", err)
	}
	if usage.LifetimeTokens == nil || *usage.LifetimeTokens != 0 {
		t.Errorf("an explicit zero must survive as a zero, got %v", usage.LifetimeTokens)
	}
	if usage.PeakDailyTokens != nil {
		t.Errorf("an omitted field must stay nil, got %v", *usage.PeakDailyTokens)
	}
	if len(usage.DailyBuckets) != 0 {
		t.Errorf("null buckets = %+v, want none", usage.DailyBuckets)
	}
	if usage.Empty() {
		t.Error("a report carrying an explicit zero is not empty")
	}

	blank, err := parseAccountUsage(json.RawMessage(`{"summary":{},"dailyUsageBuckets":[]}`))
	if err != nil {
		t.Fatalf("parseAccountUsage: %v", err)
	}
	if !blank.Empty() {
		t.Error("a report with nothing in it must read as empty")
	}
}

func TestParseAccountUsageDropsUndatedBuckets(t *testing.T) {
	usage, err := parseAccountUsage(json.RawMessage(
		`{"summary":{},"dailyUsageBuckets":[{"startDate":"  ","tokens":5},{"startDate":"2026-08-02","tokens":6}]}`))
	if err != nil {
		t.Fatalf("parseAccountUsage: %v", err)
	}
	if len(usage.DailyBuckets) != 1 || usage.DailyBuckets[0].StartDate != "2026-08-02" {
		t.Fatalf("buckets = %+v, want only the dated one", usage.DailyBuckets)
	}
}

func TestParseAccountUsageRejectsGarbage(t *testing.T) {
	if _, err := parseAccountUsage(json.RawMessage(`not json`)); err == nil {
		t.Fatal("a malformed response must be an error, not an empty report")
	}
}

// TestClassifyAccountUsageError covers the split between "there is nothing to
// report" (render no section) and a real failure (surface it).
func TestClassifyAccountUsageError(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantUnavailable bool
	}{
		{
			name: "nil stays nil",
		},
		{
			name:            "an older binary has no such method",
			err:             &RPCError{Method: accountUsageMethod, Code: -32600, Message: "Invalid request: unknown variant `account/usage/read`, expected one of ..."},
			wantUnavailable: true,
		},
		{
			name:            "method not found",
			err:             &RPCError{Method: accountUsageMethod, Code: -32601, Message: "Method not found"},
			wantUnavailable: true,
		},
		{
			name:            "an api-key login has no usage profile",
			err:             &RPCError{Method: accountUsageMethod, Code: -32600, Message: "chatgpt authentication required to read token usage"},
			wantUnavailable: true,
		},
		{
			name:            "signed out",
			err:             &RPCError{Method: accountUsageMethod, Code: -32600, Message: "codex account authentication required to read token usage"},
			wantUnavailable: true,
		},
		{
			name: "a backend failure is a real failure",
			err:  &RPCError{Method: accountUsageMethod, Code: -32603, Message: "token usage profile fetch timed out"},
		},
		{
			name: "an unrelated unknown variant is not this method going missing",
			err:  &RPCError{Method: accountUsageMethod, Code: -32600, Message: "Invalid request: unknown variant `somethingElse`, expected one of ..."},
		},
		{
			name: "a transport error is a real failure",
			err:  errors.New("read: broken pipe"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAccountUsageError(tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("classify(nil) = %v", got)
				}
				return
			}
			if errors.Is(got, ErrAccountUsageUnavailable) != tc.wantUnavailable {
				t.Fatalf("classify(%v) = %v, want unavailable=%v", tc.err, got, tc.wantUnavailable)
			}
		})
	}
}

func TestMatchAccountUsageFrame(t *testing.T) {
	if _, matched, _ := matchAccountUsageFrame([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)); matched {
		t.Error("the initialize reply must not be claimed by the usage read")
	}
	if _, matched, _ := matchAccountUsageFrame([]byte(`{"jsonrpc":"2.0","method":"someNotification"}`)); matched {
		t.Error("a notification must not be claimed")
	}
	if _, matched, _ := matchAccountUsageFrame([]byte(`garbage`)); matched {
		t.Error("a non-JSON line must not be claimed")
	}

	raw, matched, err := matchAccountUsageFrame([]byte(`{"jsonrpc":"2.0","id":2,"result":{"summary":{"lifetimeTokens":5}}}`))
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v, want the usage response", matched, err)
	}
	usage, err := parseAccountUsage(raw)
	if err != nil || usage.LifetimeTokens == nil || *usage.LifetimeTokens != 5 {
		t.Fatalf("parsed = %+v err = %v", usage, err)
	}

	_, matched, err = matchAccountUsageFrame([]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"Method not found"}}`))
	if !matched {
		t.Fatal("an error frame for the usage request must be claimed")
	}
	if !IsMethodUnsupported(err, accountUsageMethod) {
		t.Fatalf("err = %v, want a typed method-unsupported error", err)
	}
}
