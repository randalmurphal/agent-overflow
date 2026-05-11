package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// Claude's `rate_limit_event` NDJSON envelope only carries `utilization`
// once usage crosses the warning band (>=0.89). For the steady-state
// 0-88% range — which is most of the time — the wire is silent. The
// real percentages live on the HTTP response headers of the Anthropic
// Messages API:
//
//	anthropic-ratelimit-unified-5h-utilization: 0.19
//	anthropic-ratelimit-unified-5h-reset: 1778479200
//	anthropic-ratelimit-unified-5h-status: allowed
//	anthropic-ratelimit-unified-7d-utilization: 0.45
//	anthropic-ratelimit-unified-7d-reset: 1778814000
//	anthropic-ratelimit-unified-7d-status: allowed
//
// Headers are undocumented but stable across the wider Claude Code
// ecosystem. To read them, we POST a minimal 1-token Haiku request
// with the user's OAuth bearer (from ~/.claude/.credentials.json) and
// inspect the response headers. Cost per probe is ~1 input + 1 output
// Haiku token (negligible).
//
// The probe is Claude-only: Codex carries both windows on the same
// `usage` event so its rings populate at steady state without help.

const (
	// anthropicAPIBaseURL is the production Messages API endpoint.
	anthropicAPIBaseURL = "https://api.anthropic.com"

	// rateLimitProbeModel is the cheapest model we can ping just to get
	// rate-limit response headers back. Haiku 4.5 has the same headers
	// as larger models (verified via spike) and costs the least.
	rateLimitProbeModel = "claude-haiku-4-5"

	// oauthBetaHeader is the beta flag Claude Code passes when
	// authenticating with an OAuth bearer rather than an API key.
	// Sourced from Claude Code's `src/constants/oauth.ts`.
	oauthBetaHeader = "oauth-2025-04-20"

	// anthropicVersion is the API version date string. Matches what
	// Claude Code's installed binary sends.
	anthropicVersion = "2023-06-01"

	// rateLimitProbeTimeout caps the HTTP call. Generous because the
	// probe runs on a background ticker — slow networks shouldn't break
	// it, but a hung connection also shouldn't park a goroutine
	// indefinitely.
	rateLimitProbeTimeout = 15 * time.Second
)

// claudeRateLimitHeaderPrefix is the common prefix for the response
// headers we care about. Listed as a single constant so the test
// fixtures and the parser stay aligned.
const claudeRateLimitHeaderPrefix = "Anthropic-Ratelimit-Unified-"

// ratelimitWindow pairs the header window-segment ("5h" / "7d") with
// the WindowMins value the RateLimitsSnapshot uses. Defined as a
// table so adding a new window (if Anthropic ever ships one) is one
// row plus a frontend ring.
var ratelimitWindows = []struct {
	headerSegment string
	windowMins    int
	limitID       string
}{
	{headerSegment: "5h", windowMins: 300, limitID: "five_hour"},
	{headerSegment: "7d", windowMins: 10080, limitID: "seven_day"},
}

// ProbeRateLimits POSTs a 1-token request to api.anthropic.com/v1/messages
// using the user's OAuth bearer and returns a RateLimitsSnapshot parsed
// from the `anthropic-ratelimit-unified-*` response headers.
//
// Returns ErrNoCredentials when ~/.claude/.credentials.json is missing
// (user hasn't run `claude login` yet) — callers should treat this as
// "probe not available" rather than a hard error.
//
// Returns a wrapped error for transport failures, non-2xx responses,
// or missing headers (the API contract was modified upstream). Errors
// are logged at the app layer; the global rate-limit store keeps its
// last-known value rather than wiping on a transient failure.
func ProbeRateLimits(ctx context.Context, httpClient *http.Client) (provider.RateLimitsSnapshot, error) {
	bearer, err := loadOAuthBearer()
	if err != nil {
		return provider.RateLimitsSnapshot{}, err
	}

	probeCtx, cancel := context.WithTimeout(ctx, rateLimitProbeTimeout)
	defer cancel()

	headers, err := executeRateLimitsProbe(probeCtx, httpClient, bearer)
	if err != nil {
		return provider.RateLimitsSnapshot{}, err
	}

	return parseRateLimitsFromHeaders(headers, time.Now())
}

// ErrNoCredentials signals that ~/.claude/.credentials.json is missing
// or unreadable. Distinct from a network/HTTP error so callers can log
// at debug level (expected when the user hasn't authenticated yet)
// rather than at error level (a real probe failure).
var ErrNoCredentials = errors.New("claude: oauth credentials not found")

// loadOAuthBearer reads the access token from the standard Claude Code
// credentials file. Returns ErrNoCredentials when the file is missing
// or contains no token; returns a wrapped error for malformed JSON.
func loadOAuthBearer() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("claude: locate home dir: %w", err)
	}
	path := filepath.Join(home, ".claude", ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNoCredentials
		}
		return "", fmt.Errorf("claude: read credentials %s: %w", path, err)
	}

	var creds struct {
		ClaudeAIOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("claude: parse credentials: %w", err)
	}
	token := strings.TrimSpace(creds.ClaudeAIOauth.AccessToken)
	if token == "" {
		return "", ErrNoCredentials
	}
	return token, nil
}

// executeRateLimitsProbe runs the HTTP POST and returns the response
// headers on success. The response body is fully drained so the
// underlying connection can be reused.
func executeRateLimitsProbe(ctx context.Context, httpClient *http.Client, bearer string) (http.Header, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	// Minimal body: one user message, one max output token. Anthropic
	// still returns the rate-limit headers on responses this small,
	// confirmed via spike. We never read the model output.
	body := map[string]any{
		"model":      rateLimitProbeModel,
		"max_tokens": 1,
		"messages": []map[string]any{
			{"role": "user", "content": "."},
		},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("claude: marshal probe body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicAPIBaseURL+"/v1/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("claude: build probe request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("anthropic-beta", oauthBetaHeader)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude: probe request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	// Anthropic returns the rate-limit headers on both success AND
	// 429-style errors, so we don't need a 2xx to read them. But 401
	// (auth failure) means the bearer is dead and we can't proceed —
	// surface it explicitly so the app can stop firing the probe.
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("claude: probe unauthorized (bearer expired or invalid)")
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("claude: probe server error: %s", resp.Status)
	}

	return resp.Header, nil
}

// parseRateLimitsFromHeaders walks the known window segments and pulls
// utilization + reset into a RateLimitsSnapshot. A window is included
// only when BOTH its utilization and reset headers parse cleanly;
// partial windows are silently dropped (the surviving window still
// renders correctly).
//
// An empty Limits slice means none of the expected headers were
// present — usually a sign the API contract changed upstream. Returns
// an error so the caller can log loudly rather than silently emit an
// empty snapshot that would not change anything in the store.
func parseRateLimitsFromHeaders(headers http.Header, now time.Time) (provider.RateLimitsSnapshot, error) {
	entries := make([]provider.RateLimitEntry, 0, len(ratelimitWindows))
	for _, w := range ratelimitWindows {
		utilHeader := claudeRateLimitHeaderPrefix + w.headerSegment + "-Utilization"
		resetHeader := claudeRateLimitHeaderPrefix + w.headerSegment + "-Reset"

		utilStr := headers.Get(utilHeader)
		resetStr := headers.Get(resetHeader)
		if utilStr == "" || resetStr == "" {
			continue
		}

		util, err := strconv.ParseFloat(utilStr, 64)
		if err != nil {
			continue
		}
		reset, err := strconv.ParseInt(resetStr, 10, 64)
		if err != nil {
			continue
		}

		entries = append(entries, provider.RateLimitEntry{
			LimitID:     w.limitID,
			LimitName:   w.limitID,
			UsedPercent: util * 100,
			WindowMins:  w.windowMins,
			ResetsAt:    reset,
		})
	}

	if len(entries) == 0 {
		return provider.RateLimitsSnapshot{}, fmt.Errorf("claude: probe response missing rate-limit headers")
	}

	return provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		Limits:    entries,
		UpdatedAt: now.UnixMilli(),
	}, nil
}
