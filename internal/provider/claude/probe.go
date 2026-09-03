package claude

import (
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
	Binary string // default: "claude"
	// WorkDir is the probe subprocess's working directory. REQUIRED, and
	// must be absolute — see provider.ValidateProbeWorkDir for why an
	// inherited cwd is not an acceptable default here.
	WorkDir string
	Env     map[string]string
	Timeout time.Duration // default: defaultProbeTimeout

	// ReadCredential reads the credential the CLI this probe spawns will
	// authenticate with — the canonical native store, or whatever home
	// Env pins. It is called before the spawn and repeatedly during
	// teardown; it must be free of side effects and must not mutate what it
	// reads. Only a digest of the bytes is retained.
	//
	// Wiring it is what keeps the probe from destroying the account's login.
	// Spawning a CLI against a credential that is at or near expiry starts a
	// token rotation the CLI does NOT finish before answering, and Anthropic
	// retires the old refresh token the moment the request is processed — so
	// tearing the probe down on its answer loses the replacement pair for
	// good. With a reader wired, the probe holds the process open until the
	// rotation is durable; see rotationWatch for the measured window.
	//
	// nil disables that protection. It is not an error — a probe against a
	// home whose credential this package cannot locate is still a useful
	// probe — but it should be nil only where no rotation is possible.
	ReadCredential func() ([]byte, error)

	// RotationExpected declares that the caller already knows this
	// invocation will rotate the token, overriding what the credential's
	// expiry suggests. Set it when the server has just rejected the bearer:
	// the CLI's 401 recovery forces a refresh that skips every expiry gate,
	// so bytes that look fresh still rotate. See armRotationWatch.
	RotationExpected bool

	// OnModels, when non-nil, fires exactly once on a successful probe with
	// the `models` array carried by the same initialize response that produced
	// the AccountInfo. It exists so the model catalog can be enriched from the
	// zero-token probe AO already runs, rather than from a second subprocess —
	// the Claude counterpart of codex.ProbeConfig.OnSnapshot.
	//
	// The two arguments are distinct answers and callers must treat them so:
	//
	//   - (models, nil) — the CLI reported this list. What an EMPTY list (or
	//     a list missing a previously reported model) means is the CONSUMER's
	//     policy call, not this contract's: the array is a server-gated
	//     picker shortlist, so one degraded answer from the same binary is
	//     not proof a model is gone. internal/claudemodels retains models
	//     learned earlier from the same binary and subtracts only when the
	//     binary's version changes (Catalog.DropBinary).
	//   - (nil, err) — the array was present but unreadable. That is no
	//     information at all, so a consumer must keep what it had and surface
	//     the error. It is deliberately NOT fatal to the probe: identity is
	//     what the probe exists for, and a malformed cosmetic sub-field must
	//     not report a logged-in user as broken.
	OnModels func(models []WireModel, err error)

	// OnCommands, when non-nil, fires exactly once on a successful probe with
	// the `commands` array the same initialize response carries — the rich
	// {name, description, argumentHint} list of every command the CLI executes
	// itself. Same free-ride rationale as OnModels: no second subprocess.
	//
	// The two arguments follow OnModels' contract exactly:
	//
	//   - (commands, nil) — the CLI reported this list. An EMPTY list is a real
	//     answer (a CLI too old to report commands) and must replace whatever
	//     the consumer held.
	//   - (nil, err) — the array was present but unreadable. That is no
	//     information, so the consumer keeps what it had. Non-fatal to the
	//     probe: identity is what the probe exists for.
	OnCommands func(commands []provider.SlashCommand, err error)
}

// defaultProbeTimeout is the per-spawn deadline. Deliberately generous, and
// it is a deadline rather than a sleep — raising it costs an authenticated
// host nothing, while a timed-out probe fails the operation that needed it
// (a send, a login, a usage refresh) and reports a working account as broken.
//
// 25s rather than something tighter because the wall-clock cost of answering
// the initialize control_request is not bounded by our own work: a cold CLI
// start (node boot, plugins, hooks) is only the floor, and external-credential
// backends add a full credential exchange on top. On Bedrock-style setups the
// SDK runs its credential-refresh hook (`awsAuthRefresh`) before it can reply
// at all, which routinely pushes first-probe latency past the double-digit
// mark on a cold or slow host. t3-code raised the same constant to 25s for
// this exact failure.
const defaultProbeTimeout = 25 * time.Second

// DefaultProbeTTL is how long a successful probe result stays cached for a
// given binary path.
const DefaultProbeTTL = 5 * time.Minute

// probeInitRequestID is the request_id we send for the probe's
// initialize control_request. Fixed because the probe is one-shot — no
// concurrency, no need for a sequence allocator.
const probeInitRequestID = "ao-probe-init"

// ProbeAccount spawns a short-lived `claude --max-turns 0` subprocess,
// sends a `control_request{subtype:"initialize"}` as the first wire
// message, reads the matching `control_response`, and returns the
// authenticated account info from the embedded `account` object.
//
// The zero-token property of this probe is that it writes ONLY the
// initialize control_request and never a user message. `--max-turns 0`
// is NOT a backstop: spike-verified on CLI 2.1.219 that a user message
// sent under `--max-turns 0` still runs a full billed turn. Any change
// that writes additional lines to this process must re-establish the
// no-inference property itself. The account data is on the
// control_response payload (verified live — `system/init` does NOT
// carry `account` fields), so this probe does NOT depend on a
// system/init line being emitted.
//
// A zero-value AccountInfo with nil error is a valid result when the
// CLI returns success but the account object is empty (older CLI
// versions or unauthenticated environments).
func ProbeAccount(ctx context.Context, cfg ProbeConfig) (provider.AccountInfo, error) {
	if err := provider.ValidateProbeWorkDir("claude", cfg.WorkDir); err != nil {
		return provider.AccountInfo{}, err
	}
	binary := cfg.Binary
	if binary == "" {
		binary = "claude"
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}

	// Armed before the spawn, because the CLI can begin rotating the moment
	// it starts: the "before" digest has to predate the process.
	watch := armRotationWatch(cfg.ReadCredential, cfg.RotationExpected, time.Now())

	// Two deadlines, because they answer different questions. The process
	// lifetime has to cover the probe AND any rotation the probe set off,
	// and it is deliberately detached from the caller's cancellation when a
	// rotation is expected — a context cancel mid-rotation is precisely the
	// kill this budget exists to prevent, and it is still hard-bounded.
	// The read below keeps the caller's cancellation, so an aborted probe
	// still returns promptly; only the teardown waits.
	lifetime := ctx
	if watch.budget() > 0 {
		lifetime = context.WithoutCancel(ctx)
	}
	spawnCtx, cancelSpawn := context.WithTimeout(lifetime, timeout+watch.budget())
	defer cancelSpawn()
	readCtx, cancelRead := context.WithTimeout(ctx, timeout)
	defer cancelRead()

	proc, err := provider.Spawn(spawnCtx, provider.SpawnConfig{
		Binary:   binary,
		Args:     buildProbeArgs(),
		Dir:      cfg.WorkDir,
		Env:      cfg.Env,
		UnsetEnv: []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"},
		// The probe runs under a deadline and drives the CLI's own token
		// refresh; a SIGKILL between the token endpoint answering and the
		// credential write ends the account's chain (see GracefulCancel).
		GracefulCancel: true,
	})
	if err != nil {
		return provider.AccountInfo{}, fmt.Errorf("claude: probe spawn: %w", err)
	}
	// Settle BEFORE Close on every exit path, successful or not: Close starts
	// by closing stdin, and under `--max-turns 0` that is what makes the CLI
	// exit. A probe that failed or timed out can have started a rotation just
	// as easily as one that answered.
	defer func() {
		settleCtx, cancelSettle := context.WithTimeout(spawnCtx, rotationSettleTimeout)
		watch.settle(settleCtx)
		cancelSettle()
		_ = proc.Close()
	}()

	req := map[string]any{
		"type":       "control_request",
		"request_id": probeInitRequestID,
		"request":    map[string]any{"subtype": "initialize"},
	}
	reqLine, err := json.Marshal(req)
	if err != nil {
		return provider.AccountInfo{}, fmt.Errorf("claude: probe marshal initialize: %w", err)
	}
	if err := proc.WriteLine(reqLine); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("claude: probe write initialize: %w", err)
	}

	response, err := readControlInitResponse(readCtx, proc)
	if err != nil {
		return provider.AccountInfo{}, err
	}
	if cfg.OnModels != nil {
		cfg.OnModels(response.Models, response.ModelsErr)
	}
	if cfg.OnCommands != nil {
		cfg.OnCommands(response.Commands, response.CommandsErr)
	}
	return response.Account, nil
}

// buildProbeArgs returns the CLI flags used by ProbeAccount. Kept
// separate so the flag set is visible and testable without running a
// full session. `--max-turns 0` expresses intent but does not enforce
// it — see the ProbeAccount doc comment for the real no-inference
// contract.
//
// `--safe-mode` (CLI 2.1.169+) skips every customization a probe must not
// trigger — hooks, plugins, MCP servers, CLAUDE.md discovery — while OAuth
// and the native token refresh work normally (spike-verified on 2.1.219:
// the initialize control_response still carries the full `account` object).
// `--bare` is NOT a substitute: it never reads OAuth at all, which would
// report every subscription login as unauthenticated and disable the
// refresh path this probe exists to drive.
func buildProbeArgs() []string {
	return []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--max-turns", "0",
		"--safe-mode",
	}
}

// initResponse is everything ProbeAccount reads out of one initialize
// control_response. Account is the probe's product; Models and Commands ride
// along because the same response carries them. Each cosmetic sub-field keeps
// its OWN decode error (see ProbeConfig.OnModels for why they cannot share
// one, or share the probe's): one unreadable array must not report the other,
// or the account, as broken.
type initResponse struct {
	Account     provider.AccountInfo
	Models      []WireModel
	ModelsErr   error
	Commands    []provider.SlashCommand
	CommandsErr error
}

// readControlInitResponse reads stdout lines, skips intervening system
// events (e.g. SessionStart hook envelopes), and returns the parsed
// initialize response. ReadLine runs in a
// helper goroutine so ctx cancellation can interrupt blocked reads.
func readControlInitResponse(ctx context.Context, proc *provider.Process) (initResponse, error) {
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
			return initResponse{}, fmt.Errorf("claude: probe: %w", ctx.Err())
		case r := <-ch:
			if r.err != nil {
				if errors.Is(r.err, io.EOF) {
					return initResponse{}, fmt.Errorf("claude: probe: CLI exited before emitting initialize response")
				}
				return initResponse{}, fmt.Errorf("claude: probe read: %w", r.err)
			}
			if len(r.line) == 0 {
				continue
			}
			parsed, matched, err := tryParseControlInitResponse(r.line)
			if err != nil {
				return initResponse{}, err
			}
			if !matched {
				// Some other envelope (system event, hook, etc.); keep reading.
				continue
			}
			return parsed, nil
		}
	}
}

// tryParseControlInitResponse inspects one NDJSON line. Returns
// (parsed, true, nil) when the line is the matching control_response,
// (zero, false, nil) for any other envelope, and (zero, false, err)
// when the matching response carries a non-success subtype (auth
// failure, etc) or its payload cannot be read.
func tryParseControlInitResponse(line []byte) (initResponse, bool, error) {
	var envelope struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Response  json.RawMessage `json:"response"`
			Error     string          `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		// Non-JSON or unrelated envelope (e.g. some debug logs). Skip.
		return initResponse{}, false, nil
	}
	if envelope.Type != "control_response" || envelope.Response.RequestID != probeInitRequestID {
		return initResponse{}, false, nil
	}
	if envelope.Response.Subtype != "success" {
		msg := envelope.Response.Error
		if msg == "" {
			msg = envelope.Response.Subtype
		}
		return initResponse{}, true, fmt.Errorf("claude: probe initialize: %s", msg)
	}

	account, err := extractAccountInfoFromInitResponse(envelope.Response.Response)
	if err != nil {
		return initResponse{}, true, err
	}
	models, modelsErr := decodeWireModels(envelope.Response.Response)
	if modelsErr != nil {
		modelsErr = fmt.Errorf("claude: probe initialize: decode models: %w", modelsErr)
	}
	commands, commandsErr := decodeWireCommands(envelope.Response.Response)
	if commandsErr != nil {
		commandsErr = fmt.Errorf("claude: probe initialize: decode commands: %w", commandsErr)
	}
	return initResponse{
		Account:     account,
		Models:      models,
		ModelsErr:   modelsErr,
		Commands:    commands,
		CommandsErr: commandsErr,
	}, true, nil
}

// extractAccountInfoFromInitResponse decodes the `account` object out
// of the inner `response.response` payload returned by the CLI's
// initialize handler. The wire shape (verified via spike against the
// real CLI) is:
//
//	{"type":"control_response",
//	 "response":{"subtype":"success","request_id":"…","response":{
//	    "commands":[…],"agents":[…],"models":[…],
//	    "account":{"email":"…","organization":"…","subscriptionType":"Claude Max",
//	               "apiProvider":"firstParty","tokenSource":"…?"},
//	    …}}}
//
// A missing `account` field yields a zero-value AccountInfo (legitimate
// when the CLI is unauthenticated). An `account` that is present but
// unreadable is an error rather than a zero value: reporting "nobody is
// logged in" from a payload we failed to parse is the one wrong answer this
// probe can give — it drives the login banner and the OAuth refresh path.
func extractAccountInfoFromInitResponse(payload json.RawMessage) (provider.AccountInfo, error) {
	if len(payload) == 0 {
		return provider.AccountInfo{}, nil
	}
	var inner struct {
		Account struct {
			Email            string `json:"email"`
			Organization     string `json:"organization"`
			SubscriptionType string `json:"subscriptionType"`
			TokenSource      string `json:"tokenSource"`
			APIProvider      string `json:"apiProvider"`
		} `json:"account"`
	}
	if err := json.Unmarshal(payload, &inner); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("claude: probe initialize: decode account: %w", err)
	}
	return provider.AccountInfo{
		Email:            inner.Account.Email,
		SubscriptionType: inner.Account.SubscriptionType,
		TokenSource:      inner.Account.TokenSource,
		APIProvider:      inner.Account.APIProvider,
		// The wire carries the organization's display NAME only; the
		// stable uuid lives in ~/.claude.json's oauthAccount and is
		// enriched at adoption time (see provider.AccountInfo.OrgID).
		OrgName: inner.Account.Organization,
	}, nil
}

// ProbeCache aliases the shared `provider.ProbeCache` so existing callers
// keep working. All cache logic lives in `internal/provider/probecache.go`.
type ProbeCache = provider.ProbeCache

// NewProbeCache returns a fresh cache with the given entry lifetime.
// Thin wrapper around `provider.NewProbeCache` for call-site symmetry.
func NewProbeCache(ttl time.Duration) *ProbeCache {
	return provider.NewProbeCache(ttl)
}
