// Codex account probe: spawns a short-lived `codex app-server`, runs
// the JSON-RPC initialize handshake, calls `account/rateLimits/read`,
// and returns a normalized AccountInfo whose SubscriptionType carries
// the wire's `planType` value (e.g. "pro", "plus", "team").
//
// The `account/rateLimits/read` method is part of the v2 app-server
// protocol (codex-rs/app-server-protocol/src/protocol/common.rs:839)
// and returns a `RateLimitSnapshot` with `planType` populated for
// authenticated ChatGPT-plan accounts. The single-bucket top-level
// `rateLimits` field (NOT `rateLimitsByLimitId`) is the documented
// "backward-compatible single-bucket view" — that's what we read so
// the result reflects the user's general account quota rather than
// per-bucket variants like `codex_bengalfox` (Codex Spark).
//
// Probe is one-shot: a fresh `codex app-server` instance, no thread
// open, no inference. We never call `thread/start` or `thread/resume`,
// so no model tokens are consumed.

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"agent-overflow/internal/provider"
)

// ProbeConfig customizes a short-lived account probe invocation.
type ProbeConfig struct {
	Binary string // default: "codex"
	// WorkDir is the probe subprocess's working directory. REQUIRED, and
	// must be absolute — see provider.ValidateProbeWorkDir for why an
	// inherited cwd is not an acceptable default here.
	WorkDir string
	Env     map[string]string
	Timeout time.Duration // default: 20s, mirroring the Claude probe.

	// OnSnapshot, when non-nil, fires once with the rate-limit snapshot
	// extracted from the same `account/rateLimits/read` response that
	// produces AccountInfo. Lets `app_codex_probe.go` surface the
	// snapshot onto `provider:usage` so the 5h/7d rings hydrate at
	// startup — Codex TUI does the same proactive read in
	// `tui/src/app/background_requests.rs::fetch_account_rate_limits`.
	// nil = legacy behavior (snapshot discarded). The callback fires
	// only when `buildRateLimitsSnapshot` returns ok=true; empty or
	// malformed snapshots produce no call.
	OnSnapshot func(provider.RateLimitsSnapshot)
}

const (
	// defaultProbeTimeout shares the Claude probe's rationale — a cold
	// app-server start can take double-digit seconds on a slow host, and a
	// timed-out probe fails the operation that needed it, so tight is worse
	// than slow. The value is deliberately not slaved to Claude's: Claude's
	// larger deadline covers external-credential backends whose SDK runs a
	// credential-refresh hook before it can answer initialize, and no
	// equivalent pre-handshake exchange has been observed on the Codex
	// app-server. Raise this when one is.
	defaultProbeTimeout = 20 * time.Second

	// DefaultProbeTTL — process-global cache lifetime for a successful
	// probe result. Matches the Claude probe so both providers share
	// the same staleness window.
	DefaultProbeTTL = 5 * time.Minute

	probeInitializeID = 1
	probeRateLimitsID = 2
	probeAccountID    = 3
	accountReadGrace  = 150 * time.Millisecond
)

// ProbeAccount runs the JSON-RPC handshake against `codex app-server`
// and returns the authenticated account's planType in the
// SubscriptionType field. APIProvider is hardcoded to "openai" so
// downstream code that branches on apiProvider can distinguish Codex
// from Claude (whose probe sets it to "firstParty" for the direct
// Anthropic backend). TokenSource is left empty: Codex doesn't surface
// that field at all.
//
// A zero-value AccountInfo with nil error is the "succeeded but no
// plan info" outcome — observed when the user is signed in but the
// rate-limits backend hasn't seen activity yet, or when the planType
// field is absent on the wire.
func ProbeAccount(ctx context.Context, cfg ProbeConfig) (provider.AccountInfo, error) {
	if err := provider.ValidateProbeWorkDir("codex", cfg.WorkDir); err != nil {
		return provider.AccountInfo{}, err
	}
	binary := cfg.Binary
	if binary == "" {
		binary = "codex"
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	proc, err := provider.Spawn(probeCtx, provider.SpawnConfig{
		Binary:   binary,
		Args:     buildProbeArgs(),
		Dir:      cfg.WorkDir,
		Env:      cfg.Env,
		UnsetEnv: []string{"CODEX_HOME"},
		// The probe runs under a deadline and drives the CLI's own token
		// refresh; a SIGKILL between the token endpoint answering and the
		// credential write ends the account's chain (see GracefulCancel).
		GracefulCancel: true,
	})
	if err != nil {
		return provider.AccountInfo{}, fmt.Errorf("codex: probe spawn: %w", err)
	}
	defer func() { _ = proc.Close() }()

	// 1) initialize request
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      probeInitializeID,
		"method":  "initialize",
		// Response-only client: account/read plus
		// account/rateLimits/read, no notification is ever awaited.
		"params": codexInitializeParams("agent_overflow_probe", oneShotOptOutNotificationMethods()),
	}); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("codex: probe write initialize: %w", err)
	}

	// 2) initialized notification (no id, no response)
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
	}); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("codex: probe write initialized: %w", err)
	}

	// 3) account/read request. We do not wait for it independently: the
	// rate-limit response remains the completion boundary so older/fake
	// app-servers that do not implement account/read still probe cleanly.
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      probeAccountID,
		"method":  "account/read",
		"params":  map[string]any{"refreshToken": false},
	}); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("codex: probe write account/read: %w", err)
	}

	// 4) account/rateLimits/read request
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      probeRateLimitsID,
		"method":  "account/rateLimits/read",
	}); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("codex: probe write rateLimits/read: %w", err)
	}

	return readRateLimitsResponse(probeCtx, proc, cfg.OnSnapshot)
}

// buildProbeArgs returns the codex CLI flags for app-server mode.
// Kept as a function so a test can verify the invocation shape
// without spinning up a real binary.
func buildProbeArgs() []string {
	return codexAppServerArgs()
}

// writeJSONRPC marshals v as a single NDJSON line and writes it.
func writeJSONRPC(proc *provider.Process, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return proc.WriteLine(line)
}

// readRateLimitsResponse reads stdout, ignores any frames whose id
// doesn't match the rateLimits request, and returns the parsed
// account info from the first matching response. Errors on
// JSON-RPC error replies, EOF, ctx cancellation.
//
// onSnapshot, when non-nil, fires once with the rate-limit snapshot
// extracted from the same response. Once rate limits arrive, the probe gives
// account/read a short grace period to arrive out of order so saved-account
// deduplication gets the email without making older app-servers a hard
// dependency.
func readRateLimitsResponse(ctx context.Context, proc *provider.Process, onSnapshot func(provider.RateLimitsSnapshot)) (provider.AccountInfo, error) {
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

	var accountInfo provider.AccountInfo
	var rateLimitInfo provider.AccountInfo
	var rateLimitRaw json.RawMessage
	var rateLimitsReady bool
	var accountReady bool
	var graceTimer *time.Timer
	var grace <-chan time.Time
	defer func() {
		if graceTimer != nil {
			graceTimer.Stop()
		}
	}()
	finish := func() (provider.AccountInfo, error) {
		if onSnapshot != nil {
			if snap, ok := buildRateLimitsSnapshot(rateLimitRaw, time.Now().UnixMilli()); ok {
				onSnapshot(snap)
			}
		}
		if accountInfo.Email != "" {
			rateLimitInfo.Email = accountInfo.Email
		}
		if accountInfo.SubscriptionType != "" {
			rateLimitInfo.SubscriptionType = accountInfo.SubscriptionType
		}
		if accountInfo.APIProvider != "" {
			rateLimitInfo.APIProvider = accountInfo.APIProvider
		}
		return rateLimitInfo, nil
	}
	for {
		select {
		case <-ctx.Done():
			return provider.AccountInfo{}, fmt.Errorf("codex: probe: %w", ctx.Err())
		case <-grace:
			return finish()
		case r := <-ch:
			if r.err != nil {
				if rateLimitsReady {
					return finish()
				}
				if errors.Is(r.err, io.EOF) {
					return provider.AccountInfo{}, fmt.Errorf("codex: probe: app-server exited before emitting rateLimits response")
				}
				return provider.AccountInfo{}, fmt.Errorf("codex: probe read: %w", r.err)
			}
			if len(r.line) == 0 {
				continue
			}
			if info, matched := tryParseAccountResponse(r.line); matched {
				accountInfo = info
				accountReady = true
				if rateLimitsReady {
					return finish()
				}
				continue
			}
			info, raw, matched, err := tryParseRateLimitsResponse(r.line)
			if err != nil {
				return provider.AccountInfo{}, err
			}
			if !matched {
				continue
			}
			rateLimitInfo = info
			rateLimitRaw = raw
			rateLimitsReady = true
			if accountReady {
				return finish()
			}
			graceTimer = time.NewTimer(accountReadGrace)
			grace = graceTimer.C
		}
	}
}

func tryParseAccountResponse(line []byte) (provider.AccountInfo, bool) {
	var envelope struct {
		ID     *json.Number    `json:"id,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&envelope); err != nil || envelope.ID == nil {
		return provider.AccountInfo{}, false
	}
	id, err := envelope.ID.Int64()
	if err != nil || id != probeAccountID {
		return provider.AccountInfo{}, false
	}
	info, err := decodeAccountInfo(envelope.Result)
	if err != nil {
		return provider.AccountInfo{APIProvider: "openai"}, true
	}
	if info.APIProvider == "" {
		info.APIProvider = "openai"
	}
	return info, true
}

// tryParseRateLimitsResponse classifies one NDJSON frame.
// (info, raw, true, nil)  → matching successful response (raw is envelope.Result)
// (zero, nil, true, err)  → matching error response (auth failure, etc)
// (zero, nil, false, nil) → some other frame (notification, init reply, …) — keep reading
func tryParseRateLimitsResponse(line []byte) (provider.AccountInfo, json.RawMessage, bool, error) {
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
	if err := dec.Decode(&envelope); err != nil {
		// Non-JSON or unrelated — skip.
		return provider.AccountInfo{}, nil, false, nil
	}
	if envelope.ID == nil {
		return provider.AccountInfo{}, nil, false, nil
	}
	id, err := envelope.ID.Int64()
	if err != nil || id != probeRateLimitsID {
		return provider.AccountInfo{}, nil, false, nil
	}
	if envelope.Error != nil {
		return provider.AccountInfo{}, nil, true, fmt.Errorf("codex: probe rateLimits/read error %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	return extractAccountInfoFromRateLimits(envelope.Result), envelope.Result, true, nil
}

// extractAccountInfoFromRateLimits decodes the `rateLimits.planType`
// field out of `GetAccountRateLimitsResponse`. The wire shape (verified
// via spike against the live `codex app-server`) is:
//
//	{"rateLimits":{"limitId":"codex","limitName":null,
//	   "planType":"pro","primary":{...},"secondary":{...},
//	   "credits":{...},"rateLimitReachedType":null},
//	 "rateLimitsByLimitId":{...}}
//
// We read plan metadata from `rateLimits` (the top-level
// "backward-compatible single-bucket view"). Quota extraction separately
// retains every entry in rateLimitsByLimitId.
//
// Empty Result, missing planType, or a JSON parse error all yield a
// zero-value AccountInfo (legitimate when the backend hasn't yet
// returned plan data) — never an error.
func extractAccountInfoFromRateLimits(result json.RawMessage) provider.AccountInfo {
	if len(result) == 0 {
		return provider.AccountInfo{APIProvider: "openai"}
	}
	var payload struct {
		RateLimits struct {
			PlanType string `json:"planType"`
		} `json:"rateLimits"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return provider.AccountInfo{APIProvider: "openai"}
	}
	return provider.AccountInfo{
		SubscriptionType: payload.RateLimits.PlanType,
		APIProvider:      "openai",
	}
}

// ProbeCache aliases the shared `provider.ProbeCache` so existing callers
// keep working. All cache logic lives in `internal/provider/probecache.go`.
type ProbeCache = provider.ProbeCache

// NewProbeCache returns a fresh cache with the given entry lifetime.
// Thin wrapper around `provider.NewProbeCache` for call-site symmetry.
func NewProbeCache(ttl time.Duration) *ProbeCache {
	return provider.NewProbeCache(ttl)
}
