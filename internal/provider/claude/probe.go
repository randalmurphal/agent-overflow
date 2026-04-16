package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"agent-overflow/internal/provider"
)

// ProbeConfig customizes a short-lived account probe invocation.
type ProbeConfig struct {
	Binary  string // default: "claude"
	WorkDir string
	Timeout time.Duration // default: 8s (matches forge's CAPABILITIES_PROBE_TIMEOUT_MS)
}

// defaultProbeTimeout matches forge's `CAPABILITIES_PROBE_TIMEOUT_MS`.
const defaultProbeTimeout = 8 * time.Second

// DefaultProbeTTL is how long a successful probe result stays cached for a
// given binary path. Matches forge's subscription-probe TTL.
const DefaultProbeTTL = 5 * time.Minute

// ProbeAccount spawns a short-lived `claude --max-turns 0` subprocess, reads
// the initial `system/init` message, extracts the authenticated account
// fields, and returns a zero-token AccountInfo.  The subprocess is shut
// down via stdin close as soon as the init message has been parsed.
//
// The `--max-turns 0` flag guarantees no inference occurs: Claude aborts
// before making an API call. A zero-value AccountInfo with nil error is a
// valid result when the CLI does not emit an `account` object (older CLIs
// or unauthenticated environments).
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
	// Fire-and-forget: close pipes and reap the child on every exit path.
	defer func() {
		_ = proc.Close()
	}()

	// Send the user prompt so the CLI flushes its init message. With
	// --max-turns 0 the CLI emits `system/init` and then aborts before any
	// API call, so this costs zero tokens.
	if err := proc.WriteLine([]byte(`{"type":"user","message":{"role":"user","content":"."}}`)); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("claude: probe write prompt: %w", err)
	}

	info, err := readInitFromProc(probeCtx, proc)
	if err != nil {
		return provider.AccountInfo{}, err
	}
	return info, nil
}

// buildProbeArgs returns the CLI flags used by ProbeAccount. Kept separate
// from buildArgs so the probe's zero-token guarantee is visible and testable
// without running a full session.
func buildProbeArgs() []string {
	return []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--max-turns", "0",
	}
}

// readInitFromProc reads stdout lines until the first `system/init` message
// appears, respecting ctx cancellation. Runs ReadLine in a helper goroutine
// so the ctx can interrupt blocked reads.
func readInitFromProc(ctx context.Context, proc *provider.Process) (provider.AccountInfo, error) {
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
					return provider.AccountInfo{}, fmt.Errorf("claude: probe: CLI exited before emitting init")
				}
				return provider.AccountInfo{}, fmt.Errorf("claude: probe read: %w", r.err)
			}
			if len(r.line) == 0 {
				continue
			}
			isInit, err := lineIsSystemInit(r.line)
			if err != nil {
				// Non-JSON or unparseable line — skip and continue.
				continue
			}
			if !isInit {
				continue
			}
			return extractAccountInfoFromInit(r.line)
		}
	}
}

// lineIsSystemInit returns true when the given NDJSON line is the
// `system/init` message. Other system subtypes and non-system messages
// return false.
func lineIsSystemInit(line []byte) (bool, error) {
	var envelope struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return false, err
	}
	return envelope.Type == "system" && envelope.Subtype == "init", nil
}

// extractAccountInfoFromInit parses the `account` object (when present) and
// the top-level model / version fields from a system/init line.  A missing
// `account` field is not an error — it returns a zero-value AccountInfo
// with whatever session metadata the CLI did provide.
func extractAccountInfoFromInit(line []byte) (provider.AccountInfo, error) {
	var payload struct {
		Model   string `json:"model"`
		Version string `json:"claude_code_version"`
		Account struct {
			SubscriptionType string `json:"subscriptionType"`
			TokenSource      string `json:"tokenSource"`
			APIProvider      string `json:"apiProvider"`
		} `json:"account"`
	}
	if err := json.Unmarshal(line, &payload); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("claude: probe parse init: %w", err)
	}

	return provider.AccountInfo{
		SubscriptionType: payload.Account.SubscriptionType,
		TokenSource:      payload.Account.TokenSource,
		APIProvider:      payload.Account.APIProvider,
		Model:            payload.Model,
		Version:          payload.Version,
	}, nil
}

// ProbeCache stores recent AccountInfo results keyed by binary path.
// Results older than TTL are considered stale. Safe for concurrent use.
type ProbeCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]probeCacheEntry
}

type probeCacheEntry struct {
	info    provider.AccountInfo
	storeAt time.Time
}

// NewProbeCache returns a fresh cache with the given entry lifetime.
func NewProbeCache(ttl time.Duration) *ProbeCache {
	return &ProbeCache{
		ttl:     ttl,
		entries: make(map[string]probeCacheEntry),
	}
}

// Get returns a cached AccountInfo for the binary path, if present and
// not expired.
func (c *ProbeCache) Get(key string) (provider.AccountInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return provider.AccountInfo{}, false
	}
	if time.Since(entry.storeAt) > c.ttl {
		delete(c.entries, key)
		return provider.AccountInfo{}, false
	}
	return entry.info, true
}

// Set stores an AccountInfo under the given binary path key.
func (c *ProbeCache) Set(key string, info provider.AccountInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = probeCacheEntry{info: info, storeAt: time.Now()}
}
