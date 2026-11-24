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
	"strings"
	"time"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
)

// ProbeConfig customizes a short-lived account probe invocation.
type ProbeConfig struct {
	Binary string // default: "codex"
	// Reaper ends the process if the backend dies first. Required.
	Reaper procutil.Reaper
	// WorkDir is the parent of the temporary account working directory. REQUIRED, and
	// must be absolute — see provider.ValidateProbeWorkDir for why an
	// inherited cwd is not an acceptable default here.
	WorkDir string
	Env     provider.EnvInput
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
	// OnResetCredits, when non-nil, fires once with the earned-reset summary
	// of the same `account/rateLimits/read` answer: nil when that answer
	// carried none. It does not fire when no answer arrived.
	OnResetCredits func(*ResetCredits)
	// BeforeConsume is required by SpendReset and unused otherwise. It is
	// called with the account/read answer immediately before the consume is
	// written, and the consume carries the key and credit it returns. An
	// error sends nothing and is SpendReset's error, so a spend goes out only
	// under the login and the record the caller admitted.
	BeforeConsume func(provider.AccountInfo) (ResetSpend, error)
	// SendBy is required by SpendReset and unused otherwise. Every step
	// before the consume ends by it, BeforeConsume included: a consume not
	// written by then is not written at all (ErrResetLate). The steps after
	// the write have ResetSettleTimeout from the write.
	SendBy time.Time
	// OnConsumeWritten, when non-nil, is called once the consume's write was
	// attempted, written whole or not, before its answer is awaited.
	OnConsumeWritten func()
	// OnConsumeAnswer, when non-nil, is called with the consume's outcome as
	// soon as it arrives, before the rate-limit read after it.
	OnConsumeAnswer func(outcome string)
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

	// ResetSettleTimeout bounds a spend from the consume's write to its end:
	// the app-server's own 10 s bound on the backend call, then the
	// rate-limit read after it.
	ResetSettleTimeout = 20 * time.Second

	// DefaultProbeTTL — process-global cache lifetime for a successful
	// probe result. Matches the Claude probe so both providers share
	// the same staleness window.
	DefaultProbeTTL = 5 * time.Minute

	probeInitializeID = 1
	probeRateLimitsID = 2
	probeAccountID    = 3
	probeConsumeID    = 4
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
	info, _, err := runAccountProbe(ctx, cfg, false)
	return info, err
}

// SpendReset spends one earned rate-limit reset and then reads the account
// exactly as ProbeAccount does, in the same app-server, so the read after the
// spend is the refetch the protocol asks for.
//
// The wire order is account/read, then the consume with the spend
// BeforeConsume returned for that answer, then account/rateLimits/read.
// cfg.Timeout does not apply: SendBy and ResetSettleTimeout bound it.
//
// The outcome is set whenever the consume was answered with one, whatever the
// error says about the read after it. With no outcome the error is the
// consume's: *ResetRefusedError spent nothing, *ResetUnconfirmedError may have
// spent it, and a retry with the same idempotency key spends nothing more.
// Any other error, BeforeConsume's and ErrResetLate included, came before the
// request was written. The account/read answer is returned on every exit
// after it.
func SpendReset(ctx context.Context, cfg ProbeConfig) (string, provider.AccountInfo, error) {
	if cfg.BeforeConsume == nil || cfg.SendBy.IsZero() {
		return "", provider.AccountInfo{}, ErrResetSpendInvalid
	}
	info, outcome, err := runAccountProbe(ctx, cfg, true)
	return outcome, info, err
}

func runAccountProbe(ctx context.Context, cfg ProbeConfig, spend bool) (_ provider.AccountInfo, outcome string, retErr error) {
	binary := cfg.Binary
	if binary == "" {
		binary = "codex"
	}

	// probeCtx is the process's lifetime. prepCtx bounds the steps before a
	// spend's consume, which end by SendBy; a probe without a spend has one
	// bound for everything.
	var probeCtx, prepCtx context.Context
	if spend {
		var cancel, cancelPrep context.CancelFunc
		probeCtx, cancel = context.WithDeadline(ctx, cfg.SendBy.Add(ResetSettleTimeout))
		defer cancel()
		prepCtx, cancelPrep = context.WithDeadline(probeCtx, cfg.SendBy)
		defer cancelPrep()
	} else {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultProbeTimeout
		}
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
		prepCtx = probeCtx
	}

	spawnCfg, cleanup, err := prepareAccountProcess(provider.SpawnConfig{
		Binary: binary,
		Args:   buildProbeArgs(),
		Dir:    cfg.WorkDir,
		Env:    cfg.Env.Clear(provider.CodexHomeEnv),
		// The probe runs under a deadline and drives the CLI's own token
		// refresh; a SIGKILL between the token endpoint answering and the
		// credential write ends the account's chain (see GracefulCancel).
		GracefulCancel: true,
	})
	if err != nil {
		return provider.AccountInfo{}, "", err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	proc, err := provider.Spawn(probeCtx, provider.Bounded(cfg.Reaper), spawnCfg)
	if err != nil {
		return provider.AccountInfo{}, "", fmt.Errorf("codex: probe spawn: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, proc.Close()) }()

	// 1) initialize request
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      probeInitializeID,
		"method":  "initialize",
		// Response-only client: account/read plus
		// account/rateLimits/read, no notification is ever awaited.
		"params": codexInitializeParams("agent_overflow_probe", oneShotOptOutNotificationMethods()),
	}); err != nil {
		return provider.AccountInfo{}, "", fmt.Errorf("codex: probe write initialize: %w", err)
	}

	// 2) initialized notification (no id, no response)
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
	}); err != nil {
		return provider.AccountInfo{}, "", fmt.Errorf("codex: probe write initialized: %w", err)
	}

	lines := startLineReader(probeCtx, proc)

	// 3) a spend first reads whose login this is, and sends the consume only
	// with what the caller admits for it; its answer comes before the
	// rate-limit read, so that read describes the account after it.
	var confirmed *provider.AccountInfo
	if spend {
		if err := writeAccountRead(proc); err != nil {
			return provider.AccountInfo{}, "", err
		}
		info, err := awaitAccountResponse(prepCtx, lines)
		if err != nil {
			return provider.AccountInfo{}, "", err
		}
		confirmed = &info
		admitted, err := cfg.BeforeConsume(info)
		if err != nil {
			return info, "", err
		}
		if strings.TrimSpace(admitted.IdempotencyKey) == "" || strings.TrimSpace(admitted.CreditID) == "" {
			return info, "", ErrResetSpendInvalid
		}
		// The last check before the write, with nothing that can wait
		// between them: BeforeConsume may have run past the cutoff.
		if !time.Now().Before(cfg.SendBy) {
			return info, "", ErrResetLate
		}
		writeErr := writeJSONRPC(proc, map[string]any{
			"jsonrpc": "2.0",
			"id":      probeConsumeID,
			"method":  "account/rateLimitResetCredit/consume",
			"params":  map[string]any{"idempotencyKey": admitted.IdempotencyKey, "creditId": admitted.CreditID},
		})
		if cfg.OnConsumeWritten != nil {
			cfg.OnConsumeWritten()
		}
		if writeErr != nil {
			// A write that failed part way may still have reached the
			// app-server.
			return info, "", &ResetUnconfirmedError{Cause: fmt.Errorf("write consume: %w", writeErr)}
		}
		settleCtx, cancelSettle := context.WithTimeout(probeCtx, ResetSettleTimeout)
		defer cancelSettle()
		probeCtx = settleCtx
		outcome, err = awaitConsumeResponse(probeCtx, lines)
		if err != nil {
			return info, "", err
		}
		if cfg.OnConsumeAnswer != nil {
			cfg.OnConsumeAnswer(outcome)
		}
	}

	// 4) account/read request, unless a spend already read it. We do not
	// wait for it independently: the rate-limit response remains the
	// completion boundary so older/fake app-servers that do not implement
	// account/read still probe cleanly.
	if confirmed == nil {
		if err := writeAccountRead(proc); err != nil {
			return provider.AccountInfo{}, outcome, err
		}
	}

	// 5) account/rateLimits/read request
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      probeRateLimitsID,
		"method":  "account/rateLimits/read",
	}); err != nil {
		return accountRead(confirmed), outcome, fmt.Errorf("codex: probe write rateLimits/read: %w", err)
	}

	info, err := readRateLimitsResponse(probeCtx, lines, confirmed, cfg.OnSnapshot, cfg.OnResetCredits)
	return info, outcome, err
}

func accountRead(info *provider.AccountInfo) provider.AccountInfo {
	if info == nil {
		return provider.AccountInfo{}
	}
	return *info
}

func writeAccountRead(proc *provider.Process) error {
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      probeAccountID,
		"method":  "account/read",
		"params":  map[string]any{"refreshToken": false},
	}); err != nil {
		return fmt.Errorf("codex: probe write account/read: %w", err)
	}
	return nil
}

// awaitAccountResponse reads up to the account/read answer. An answer that
// names no login reads as a zero identity, for the caller to refuse.
func awaitAccountResponse(ctx context.Context, lines <-chan readResult) (provider.AccountInfo, error) {
	for {
		select {
		case <-ctx.Done():
			return provider.AccountInfo{}, fmt.Errorf("codex: probe: no account/read answer: %w", ctx.Err())
		case r := <-lines:
			if r.err != nil {
				return provider.AccountInfo{}, fmt.Errorf("codex: probe: app-server ended before the account/read answer: %w", r.err)
			}
			if info, matched := tryParseAccountResponse(r.line); matched {
				return info, nil
			}
		}
	}
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

type readResult struct {
	line []byte
	err  error
}

// startLineReader reads the probe's stdout on one goroutine for the whole
// probe, so the consume wait and the rate-limit read share it. It stops after
// the first read error or when ctx ends.
func startLineReader(ctx context.Context, proc *provider.Process) <-chan readResult {
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
	return ch
}

// awaitConsumeResponse waits for the consume answer, skipping every other
// frame. Anything that ends the wait without an answer leaves the outcome
// unknown.
func awaitConsumeResponse(ctx context.Context, lines <-chan readResult) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", &ResetUnconfirmedError{Cause: fmt.Errorf("no consume answer: %w", ctx.Err())}
		case r := <-lines:
			if r.err != nil {
				return "", &ResetUnconfirmedError{Cause: fmt.Errorf("app-server ended before the consume answer: %w", r.err)}
			}
			var envelope struct {
				ID     *json.Number    `json:"id,omitempty"`
				Result json.RawMessage `json:"result,omitempty"`
				Error  *rpcError       `json:"error,omitempty"`
			}
			dec := json.NewDecoder(bytes.NewReader(r.line))
			dec.UseNumber()
			if err := dec.Decode(&envelope); err != nil || envelope.ID == nil {
				continue
			}
			if id, err := envelope.ID.Int64(); err != nil || id != probeConsumeID {
				continue
			}
			return classifyConsumeResponse(envelope.Result, envelope.Error)
		}
	}
}

// readRateLimitsResponse reads stdout, ignores any frames whose id
// doesn't match the rateLimits request, and returns the parsed
// account info from the first matching response. Errors on
// JSON-RPC error replies, EOF, ctx cancellation; an error still returns
// the account/read answer when one arrived, so the caller knows whose
// credential the app-server ran.
//
// onSnapshot, when non-nil, fires once with the rate-limit snapshot
// extracted from the same response, and onResetCredits with its earned-reset
// summary. Once rate limits arrive, the probe gives
// account/read a short grace period to arrive out of order so saved-account
// deduplication gets the email without making older app-servers a hard
// dependency.
func readRateLimitsResponse(
	ctx context.Context,
	ch <-chan readResult,
	account *provider.AccountInfo,
	onSnapshot func(provider.RateLimitsSnapshot),
	onResetCredits func(*ResetCredits),
) (provider.AccountInfo, error) {
	var accountInfo provider.AccountInfo
	var rateLimitInfo provider.AccountInfo
	var rateLimitRaw json.RawMessage
	var rateLimitsReady bool
	// account is an account/read answer already read; nil waits for one.
	accountReady := account != nil
	if accountReady {
		accountInfo = *account
	}
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
		if onResetCredits != nil {
			onResetCredits(parseResetCredits(rateLimitRaw))
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
		// Only account/read names the workspace; rateLimits never does.
		rateLimitInfo.OrgName = accountInfo.OrgName
		rateLimitInfo.OrgNameReported = accountInfo.OrgNameReported
		return rateLimitInfo, nil
	}
	for {
		select {
		case <-ctx.Done():
			return accountInfo, fmt.Errorf("codex: probe: %w", ctx.Err())
		case <-grace:
			return finish()
		case r := <-ch:
			if r.err != nil {
				if rateLimitsReady {
					return finish()
				}
				if errors.Is(r.err, io.EOF) {
					return accountInfo, fmt.Errorf("codex: probe: app-server exited before emitting rateLimits response")
				}
				return accountInfo, fmt.Errorf("codex: probe read: %w", r.err)
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
				return accountInfo, err
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
