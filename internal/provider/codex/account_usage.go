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

// ---------------------------------------------------------------------------
// Thread usage (`account/usage/read {threadId}`)
// ---------------------------------------------------------------------------

// threadUsageMinimumCodexVersion is the first codex release whose
// `account/usage/read` accepts params at all. Through 0.147 the method's
// params are typed `Option<()>`
// (codex-rs/app-server-protocol/src/protocol/common.rs `GetAccountTokenUsage`),
// so a `{"threadId": …}` request does not degrade to an account-wide read —
// it fails deserialization and comes back as a JSON-RPC error. 0.148 retyped
// them `Option<GetAccountTokenUsageParams>` and added `thread_usage` to the
// response.
//
// This is a per-METHOD floor, unrelated to the provider-wide launch floor
// (provider.minimumCodexCLIVersion, 0.143): a codex between the two is
// perfectly usable, it just cannot answer this question.
const threadUsageMinimumCodexVersion = "0.148.0"

// ErrThreadUsageUnavailable means Codex has no thread-level usage estimate to
// report. Like ErrAccountUsageUnavailable this is a STATE answer rather than a
// failure — the caller keeps whatever it already had (for AO: the
// internal/usagecost table price) instead of showing nothing. Four shapes
// reach it:
//
//   - the connected app-server predates threadUsageMinimumCodexVersion;
//   - the signed-in account is not a ChatGPT account (an API-key login has no
//     billing route);
//   - the backend answered 403/404 for this thread, which upstream maps to
//     `thread_usage: null` rather than an error (account_processor.rs) — the
//     documented "billing route is available" caveat on the field;
//   - the response carried a thread_usage object with no USD figure, i.e.
//     credits only.
var ErrThreadUsageUnavailable = errors.New("codex: thread usage unavailable")

// ThreadUsage is Codex's OWN estimate of what one thread has cost. It is
// CUMULATIVE over the whole thread, not a per-turn delta — every read
// restates the thread's lifetime total.
//
// The figure is upstream's estimate, computed by the ChatGPT backend from its
// billing route, not a settled invoice. AO stores it as the provider-reported
// total for the thread and keeps its own per-turn token accounting untouched
// alongside it (see internal/store/provider_thread_cost.go).
type ThreadUsage struct {
	// ThreadID is the Codex thread the estimate describes, echoed by the
	// backend. Callers must compare it against the thread they asked about:
	// this is the only field that can catch an estimate being attributed to
	// the wrong thread.
	ThreadID string `json:"threadId"`
	// CreditsMicros is millionths of a usage credit. Always present on the
	// wire (upstream types it a bare i64).
	CreditsMicros int64 `json:"creditsMicros"`
	// USDMicros is millionths of a US dollar, or nil when the backend priced
	// the thread in credits only. `Option<i64>` upstream, and the reason the
	// rate-table fallback stays required.
	USDMicros *int64 `json:"usdMicros,omitempty"`
	// The wire's per-(model, effort, speed) `groups` breakdown is
	// deliberately NOT projected. AO's per-model split comes from its own
	// usage_ledger, and upstream types every token field in a group
	// `Option<i64>` — so a group cannot substitute for a ledger row, which is
	// what a consumer would want it for. Projecting it produced an exported
	// slice, an exported element type, and a per-group allocation on every
	// settled turn, read only by the test that asserted the projection had
	// happened. If a consumer appears, the wire struct below still names
	// every field.
}

// USD converts USDMicros to dollars, reporting whether the backend supplied
// one at all. Never fabricates a price from CreditsMicros: the credit-to-USD
// rate is the backend's, not ours.
func (u ThreadUsage) USD() (float64, bool) {
	if u.USDMicros == nil {
		return 0, false
	}
	return float64(*u.USDMicros) / 1e6, true
}

type threadUsageResponse struct {
	ThreadUsage *struct {
		ThreadID                    string `json:"threadId"`
		EstimatedUsageCreditsMicros int64  `json:"estimatedUsageCreditsMicros"`
		EstimatedUsageUsdMicros     *int64 `json:"estimatedUsageUsdMicros"`
		// Named, not decoded: `groups` is the per-(model, effort, speed)
		// breakdown, and it is documented here so a future consumer knows
		// the shape without re-reading account_processor.rs —
		// `{model, reasoningEffort, speed, estimatedUsageCreditsMicros,
		// netNewInputTokens, cachedInputTokens, inputTokens, outputTokens,
		// totalTokens}`, every token field an `Option<i64>`. Decoding it
		// walks the array on every settled turn for a value nothing reads.
	} `json:"threadUsage"`
}

// parseThreadUsage projects the wire response for a thread-scoped
// `account/usage/read`.
//
// A thread-scoped response deliberately carries NOTHING else: upstream
// answers it with an all-nil summary and `dailyUsageBuckets: null`
// (account_processor.rs), so this never feeds the account-usage cache.
//
// An absent or null `threadUsage`, and a present one with no USD figure, both
// resolve to ErrThreadUsageUnavailable — from a caller's point of view they
// are the same answer: keep the fallback.
func parseThreadUsage(raw json.RawMessage) (ThreadUsage, error) {
	var wire threadUsageResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return ThreadUsage{}, fmt.Errorf("codex: decode %s thread response: %w", accountUsageMethod, err)
	}
	if wire.ThreadUsage == nil {
		return ThreadUsage{}, fmt.Errorf("%w: no billing route for this thread", ErrThreadUsageUnavailable)
	}
	usage := ThreadUsage{
		ThreadID:      strings.TrimSpace(wire.ThreadUsage.ThreadID),
		CreditsMicros: wire.ThreadUsage.EstimatedUsageCreditsMicros,
		USDMicros:     wire.ThreadUsage.EstimatedUsageUsdMicros,
	}
	if usage.USDMicros == nil {
		return usage, fmt.Errorf("%w: the backend priced this thread in credits only", ErrThreadUsageUnavailable)
	}
	return usage, nil
}

// classifyThreadUsageError folds the "nothing to report" answers onto
// ErrThreadUsageUnavailable. It reuses classifyAccountUsageError's auth and
// unsupported-method matching (the two share one upstream handler and one
// error surface) and re-labels the result, so a new refusal string only has
// to be recognized in one place.
func classifyThreadUsageError(err error) error {
	if err == nil {
		return nil
	}
	if classified := classifyAccountUsageError(err); errors.Is(classified, ErrAccountUsageUnavailable) {
		return fmt.Errorf("%w: %s", ErrThreadUsageUnavailable, strings.TrimPrefix(
			classified.Error(), ErrAccountUsageUnavailable.Error()+": "))
	}
	return err
}

// ReadThreadUsage asks this session's app-server what Codex itself estimates
// the session's ROOT thread has cost, cumulatively.
//
// Requires codex >= threadUsageMinimumCodexVersion; an older app-server
// returns ErrThreadUsageUnavailable without a request being sent, because on
// those builds the request is a guaranteed JSON-RPC error rather than a
// graceful degradation.
//
// Safe to call while a turn is running — the method is global
// (`serialization: None`), touches no thread state and starts no turn — but
// callers should not: the estimate only moves when a turn settles, and the
// app-server forwards every call to the ChatGPT backend.
func (s *Session) ReadThreadUsage(ctx context.Context) (ThreadUsage, error) {
	if !s.appServerAtLeast(threadUsageMinimumCodexVersion) {
		return ThreadUsage{}, fmt.Errorf(
			"%w: codex %q predates %s params on %s",
			ErrThreadUsageUnavailable, s.AppServerVersion(),
			threadUsageMinimumCodexVersion, accountUsageMethod,
		)
	}
	threadID := s.rootThreadID()
	if threadID == "" {
		return ThreadUsage{}, fmt.Errorf("%w: session has no codex thread id yet", ErrThreadUsageUnavailable)
	}
	raw, err := s.sendRequest(ctx, accountUsageMethod, map[string]any{"threadId": threadID})
	if err != nil {
		return ThreadUsage{}, classifyThreadUsageError(err)
	}
	usage, err := parseThreadUsage(raw)
	if err != nil {
		return usage, err
	}
	// The echoed id is the only defense against an estimate landing on the
	// wrong thread's row. A mismatch is a wire fault, not a state answer, so
	// it does NOT resolve to ErrThreadUsageUnavailable — the caller should see
	// it in the log rather than silently keep the fallback forever.
	if usage.ThreadID != "" && usage.ThreadID != threadID {
		return ThreadUsage{}, fmt.Errorf(
			"codex: %s returned usage for thread %q, asked for %q",
			accountUsageMethod, usage.ThreadID, threadID,
		)
	}
	return usage, nil
}

// DefaultThreadUsageTimeout bounds one thread-usage read. Deliberately above
// upstream's own THREAD_USAGE_FETCH_TIMEOUT (60s, account_processor.rs): a
// client ceiling BELOW the server's would turn slow-but-successful backend
// reads into client failures and permanently starve the estimate on exactly
// the accounts whose backend is slowest. Nothing waits on this call, so the
// only cost of the long ceiling is one parked goroutine.
const DefaultThreadUsageTimeout = 65 * time.Second
