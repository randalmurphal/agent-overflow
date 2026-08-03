package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// accountUsageMethod is Codex's report of what the signed-in ACCOUNT has
// spent — not what this app-server session spent, and not what Agent Overflow
// recorded. `GetAccountTokenUsage => "account/usage/read"` with
// `serialization: None`
// (codex-rs/app-server-protocol/src/protocol/common.rs:1019), so it is
// global: no thread, no turn, no model tokens. It is not `#[experimental]`,
// so it needs no capability opt-in either.
//
// The app-server answers it by calling the ChatGPT backend
// (account_processor.rs:946 `client.get_token_usage_profile()`), which is why
// it needs ChatGPT auth and why callers must cache rather than call it per
// render.
const accountUsageMethod = "account/usage/read"

// ErrAccountUsageUnavailable means Codex has no account usage to report —
// the binary predates the method, or the signed-in account is not a ChatGPT
// account (an API-key login has no usage profile). Both are STATE answers, not
// failures: the caller renders nothing rather than rendering zeros.
var ErrAccountUsageUnavailable = errors.New("codex: account usage unavailable")

// AccountUsage is Codex's own token-usage report for the signed-in account.
//
// Every summary field is a pointer because upstream types them
// `Option<i64>` (v2/account.rs AccountTokenUsageSummary) and the backend
// genuinely omits them for accounts it has no history for. A missing value
// is not zero — rendering "0 tokens lifetime" for "the backend didn't say"
// would be a fabricated number on a surface whose whole point is that these
// are the provider's own figures.
//
// There is deliberately no cost, no per-model split, and no input/output
// split on this wire: the response is a usage PROFILE (lifetime total, peak
// day, streaks, longest turn, per-day totals), so it complements AO's priced
// ledger rather than replacing it.
type AccountUsage struct {
	LifetimeTokens        *int64                    `json:"lifetimeTokens,omitempty"`
	PeakDailyTokens       *int64                    `json:"peakDailyTokens,omitempty"`
	LongestRunningTurnSec *int64                    `json:"longestRunningTurnSec,omitempty"`
	CurrentStreakDays     *int64                    `json:"currentStreakDays,omitempty"`
	LongestStreakDays     *int64                    `json:"longestStreakDays,omitempty"`
	DailyBuckets          []AccountUsageDailyBucket `json:"dailyBuckets"`
}

// AccountUsageDailyBucket is one day of account-wide token usage.
// StartDate is the backend's own date string ("YYYY-MM-DD" on codex-cli
// 0.146.0); days with no usage are absent from the array rather than present
// with zero.
type AccountUsageDailyBucket struct {
	StartDate string `json:"startDate"`
	Tokens    int64  `json:"tokens"`
}

// Empty reports whether the response carried nothing renderable. A response
// with no summary values and no buckets is indistinguishable from having no
// account history, so callers treat it the same as unavailable.
func (u AccountUsage) Empty() bool {
	return u.LifetimeTokens == nil &&
		u.PeakDailyTokens == nil &&
		u.LongestRunningTurnSec == nil &&
		u.CurrentStreakDays == nil &&
		u.LongestStreakDays == nil &&
		len(u.DailyBuckets) == 0
}

type accountUsageResponse struct {
	Summary struct {
		LifetimeTokens        *int64 `json:"lifetimeTokens"`
		PeakDailyTokens       *int64 `json:"peakDailyTokens"`
		LongestRunningTurnSec *int64 `json:"longestRunningTurnSec"`
		CurrentStreakDays     *int64 `json:"currentStreakDays"`
		LongestStreakDays     *int64 `json:"longestStreakDays"`
	} `json:"summary"`
	DailyUsageBuckets []struct {
		StartDate string `json:"startDate"`
		Tokens    int64  `json:"tokens"`
	} `json:"dailyUsageBuckets"`
}

// parseAccountUsage projects the wire response. Buckets with no date are
// dropped rather than rendered against an empty key.
func parseAccountUsage(raw json.RawMessage) (AccountUsage, error) {
	var wire accountUsageResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return AccountUsage{}, fmt.Errorf("codex: decode %s response: %w", accountUsageMethod, err)
	}
	usage := AccountUsage{
		LifetimeTokens:        wire.Summary.LifetimeTokens,
		PeakDailyTokens:       wire.Summary.PeakDailyTokens,
		LongestRunningTurnSec: wire.Summary.LongestRunningTurnSec,
		CurrentStreakDays:     wire.Summary.CurrentStreakDays,
		LongestStreakDays:     wire.Summary.LongestStreakDays,
		DailyBuckets:          make([]AccountUsageDailyBucket, 0, len(wire.DailyUsageBuckets)),
	}
	for _, bucket := range wire.DailyUsageBuckets {
		date := strings.TrimSpace(bucket.StartDate)
		if date == "" {
			continue
		}
		usage.DailyBuckets = append(usage.DailyBuckets, AccountUsageDailyBucket{
			StartDate: date,
			Tokens:    bucket.Tokens,
		})
	}
	return usage, nil
}

// classifyAccountUsageError maps the two "there is nothing to report"
// answers onto ErrAccountUsageUnavailable and passes everything else
// through.
//
// The auth refusals are matched on their message because upstream raises
// them as a generic `invalid_request` with no distinguishing code
// (account_processor.rs:934-944, both strings end in "to read token
// usage"). The unsupported-method case is a version answer, matched
// structurally by IsMethodUnsupported.
func classifyAccountUsageError(err error) error {
	if err == nil {
		return nil
	}
	if IsMethodUnsupported(err, accountUsageMethod) {
		return fmt.Errorf("%w: this codex build has no %s", ErrAccountUsageUnavailable, accountUsageMethod)
	}
	if strings.Contains(err.Error(), "authentication required to read token usage") {
		return fmt.Errorf("%w: the signed-in codex account has no usage profile", ErrAccountUsageUnavailable)
	}
	return err
}

// ReadAccountUsage asks a LIVE session's app-server for the account usage
// report. Preferred over the ephemeral fetcher whenever a Codex session
// happens to be open: the process, its handshake, and its credentials are
// already there, so the read costs one JSON-RPC round trip instead of a
// subprocess.
//
// Safe to call at any time, including mid-turn: the method is global
// (`serialization: None`), touches no thread state, and starts no turn.
func (s *Session) ReadAccountUsage(ctx context.Context) (AccountUsage, error) {
	raw, err := s.sendRequest(ctx, accountUsageMethod, nil)
	if err != nil {
		return AccountUsage{}, classifyAccountUsageError(err)
	}
	return parseAccountUsage(raw)
}

// DefaultAccountUsageTimeout bounds the ephemeral read. Larger than the MCP
// status fetcher's because this one is not local: the app-server makes a
// ChatGPT backend request on our behalf (upstream bounds that half with its
// own ACCOUNT_TOKEN_USAGE_FETCH_TIMEOUT), so the ceiling has to cover a cold
// binary start plus a network round trip.
const DefaultAccountUsageTimeout = 20 * time.Second

// AccountUsageFetcher runs a short-lived `codex app-server`, performs the
// initialize handshake, calls `account/usage/read`, and tears the process
// down. Used when no Codex session is live.
//
// It spawns through provider.Spawn (rather than exec directly, as the MCP
// status fetcher does) for the same reason ProbeAccount does: this is an
// ACCOUNT-scoped read, so it has to run under the same pinned credential
// environment and the same CODEX_HOME unset every other account-identity
// path uses. Env is therefore the per-provider pin map, not a whole
// environment. No `thread/start` is issued, so no turn is billed and no
// provider state is mutated.
type AccountUsageFetcher struct {
	Binary string
	// WorkDir is the subprocess's working directory. Required and absolute
	// for the same reason ProbeConfig.WorkDir is: Codex discovers
	// project-scoped configuration by walking up from its cwd, so an
	// inherited cwd would let one project's config decide whose account
	// answers.
	WorkDir string
	Env     map[string]string
	Timeout time.Duration // 0 → DefaultAccountUsageTimeout
}

const accountUsageInitializeID = 1
const accountUsageReadID = 2

// Fetch reads the account usage report from a throwaway app-server.
func (f *AccountUsageFetcher) Fetch(ctx context.Context) (AccountUsage, error) {
	if strings.TrimSpace(f.Binary) == "" {
		return AccountUsage{}, fmt.Errorf("codex account usage: binary path required")
	}
	if err := provider.ValidateProbeWorkDir("codex", f.WorkDir); err != nil {
		return AccountUsage{}, err
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultAccountUsageTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary:   f.Binary,
		Args:     codexAppServerArgs(),
		Dir:      f.WorkDir,
		Env:      f.Env,
		UnsetEnv: []string{"CODEX_HOME"},
	})
	if err != nil {
		return AccountUsage{}, fmt.Errorf("codex account usage: spawn: %w", err)
	}
	defer func() { _ = proc.Close() }()

	// Deliberately not codexInitializeParams: this process makes exactly one
	// non-experimental call, so opting a throwaway into the experimental API
	// would only change what the server is willing to emit. It still opts out
	// of every notification, since it awaits none.
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      accountUsageInitializeID,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities": map[string]any{
				"optOutNotificationMethods": oneShotOptOutNotificationMethods(),
			},
			"clientInfo": map[string]any{
				"name":    "agent_overflow_account_usage",
				"version": "0.1.0",
			},
		},
	}); err != nil {
		return AccountUsage{}, fmt.Errorf("codex account usage: write initialize: %w", err)
	}
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
	}); err != nil {
		return AccountUsage{}, fmt.Errorf("codex account usage: write initialized: %w", err)
	}
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      accountUsageReadID,
		"method":  accountUsageMethod,
	}); err != nil {
		return AccountUsage{}, fmt.Errorf("codex account usage: write %s: %w", accountUsageMethod, err)
	}

	return readAccountUsageResponse(ctx, proc)
}

// readAccountUsageResponse reads NDJSON frames until the one matching the
// usage request arrives, ignoring everything else (the initialize reply and
// any notification the opt-out list did not cover).
func readAccountUsageResponse(ctx context.Context, proc *provider.Process) (AccountUsage, error) {
	type readResult struct {
		line []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		for {
			line, err := proc.ReadLine()
			select {
			case ch <- readResult{line: line, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return AccountUsage{}, fmt.Errorf("codex account usage: %w", ctx.Err())
		case r := <-ch:
			if r.err != nil {
				if errors.Is(r.err, io.EOF) {
					return AccountUsage{}, fmt.Errorf("codex account usage: app-server exited before answering %s", accountUsageMethod)
				}
				return AccountUsage{}, fmt.Errorf("codex account usage read: %w", r.err)
			}
			raw, matched, err := matchAccountUsageFrame(r.line)
			if !matched {
				continue
			}
			if err != nil {
				return AccountUsage{}, classifyAccountUsageError(err)
			}
			return parseAccountUsage(raw)
		}
	}
}

// matchAccountUsageFrame classifies one NDJSON frame:
//
//	(result, true, nil)  → the usage response
//	(nil, true, err)     → a JSON-RPC error for the usage request
//	(nil, false, nil)    → some other frame; keep reading
func matchAccountUsageFrame(line []byte) (json.RawMessage, bool, error) {
	var envelope struct {
		ID     *json.Number    `json:"id,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&envelope); err != nil || envelope.ID == nil {
		return nil, false, nil
	}
	id, err := envelope.ID.Int64()
	if err != nil || id != accountUsageReadID {
		return nil, false, nil
	}
	if envelope.Error != nil {
		return nil, true, &RPCError{
			Method:  accountUsageMethod,
			Code:    envelope.Error.Code,
			Message: envelope.Error.Message,
		}
	}
	return envelope.Result, true, nil
}
