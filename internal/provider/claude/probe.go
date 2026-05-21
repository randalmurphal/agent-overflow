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
	Binary  string // default: "claude"
	WorkDir string
	Timeout time.Duration // default: 8s
}

// defaultProbeTimeout is the per-spawn deadline.
const defaultProbeTimeout = 8 * time.Second

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
// `--max-turns 0` is defense-in-depth: even if we somehow fail to tear
// the process down promptly, the CLI cannot perform inference. The
// account data is on the control_response payload (verified live —
// `system/init` does NOT carry `account` fields), so this probe does
// NOT depend on a system/init line being emitted.
//
// A zero-value AccountInfo with nil error is a valid result when the
// CLI returns success but the account object is empty (older CLI
// versions or unauthenticated environments).
func ProbeAccount(ctx context.Context, cfg ProbeConfig) (provider.AccountInfo, error) {
	binary := cfg.Binary
	if binary == "" {
		binary = "claude"
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	proc, err := provider.Spawn(probeCtx, provider.SpawnConfig{
		Binary: binary,
		Args:   buildProbeArgs(),
		Dir:    cfg.WorkDir,
	})
	if err != nil {
		return provider.AccountInfo{}, fmt.Errorf("claude: probe spawn: %w", err)
	}
	defer func() {
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

	return readControlInitResponse(probeCtx, proc)
}

// buildProbeArgs returns the CLI flags used by ProbeAccount. Kept
// separate so the zero-token guarantee (`--max-turns 0`) is visible
// and testable without running a full session.
func buildProbeArgs() []string {
	return []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--max-turns", "0",
	}
}

// readControlInitResponse reads stdout lines, skips intervening system
// events (e.g. SessionStart hook envelopes), and returns the parsed
// account info from the matching control_response. ReadLine runs in a
// helper goroutine so ctx cancellation can interrupt blocked reads.
func readControlInitResponse(ctx context.Context, proc *provider.Process) (provider.AccountInfo, error) {
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
			return provider.AccountInfo{}, fmt.Errorf("claude: probe: %w", ctx.Err())
		case r := <-ch:
			if r.err != nil {
				if errors.Is(r.err, io.EOF) {
					return provider.AccountInfo{}, fmt.Errorf("claude: probe: CLI exited before emitting initialize response")
				}
				return provider.AccountInfo{}, fmt.Errorf("claude: probe read: %w", r.err)
			}
			if len(r.line) == 0 {
				continue
			}
			info, matched, err := tryParseControlInitResponse(r.line)
			if err != nil {
				return provider.AccountInfo{}, err
			}
			if !matched {
				// Some other envelope (system event, hook, etc.); keep reading.
				continue
			}
			return info, nil
		}
	}
}

// tryParseControlInitResponse inspects one NDJSON line. Returns
// (info, true, nil) when the line is the matching control_response,
// (zero, false, nil) for any other envelope, and (zero, false, err)
// when the matching response carries a non-success subtype (auth
// failure, etc).
func tryParseControlInitResponse(line []byte) (provider.AccountInfo, bool, error) {
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
		return provider.AccountInfo{}, false, nil
	}
	if envelope.Type != "control_response" || envelope.Response.RequestID != probeInitRequestID {
		return provider.AccountInfo{}, false, nil
	}
	if envelope.Response.Subtype != "success" {
		msg := envelope.Response.Error
		if msg == "" {
			msg = envelope.Response.Subtype
		}
		return provider.AccountInfo{}, true, fmt.Errorf("claude: probe initialize: %s", msg)
	}
	return extractAccountInfoFromInitResponse(envelope.Response.Response), true, nil
}

// extractAccountInfoFromInitResponse decodes the `account` object out
// of the inner `response.response` payload returned by the CLI's
// initialize handler. The wire shape (verified via spike against the
// real CLI) is:
//
//	{"type":"control_response",
//	 "response":{"subtype":"success","request_id":"…","response":{
//	    "commands":[…],"agents":[…],
//	    "account":{"email":"…","subscriptionType":"Claude Max",
//	               "apiProvider":"firstParty","tokenSource":"…?"},
//	    …}}}
//
// A missing `account` field yields a zero-value AccountInfo (legitimate
// when the CLI is unauthenticated).
func extractAccountInfoFromInitResponse(payload json.RawMessage) provider.AccountInfo {
	if len(payload) == 0 {
		return provider.AccountInfo{}
	}
	var inner struct {
		Account struct {
			SubscriptionType string `json:"subscriptionType"`
			TokenSource      string `json:"tokenSource"`
			APIProvider      string `json:"apiProvider"`
		} `json:"account"`
	}
	if err := json.Unmarshal(payload, &inner); err != nil {
		return provider.AccountInfo{}
	}
	return provider.AccountInfo{
		SubscriptionType: inner.Account.SubscriptionType,
		TokenSource:      inner.Account.TokenSource,
		APIProvider:      inner.Account.APIProvider,
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
