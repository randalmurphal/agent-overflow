package claude

import (
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
// once usage crosses the warning band (>=0.89). The authenticated
// `/api/oauth/usage` endpoint is the complete source used by Claude Code's
// own `/usage` view. In addition to the familiar session and weekly limits it
// exposes dynamic scoped limits (for example a Fable-only weekly bucket)
// through a `limits[]` array. We parse that array rather than hardcoding model
// names. Older response shapes can still be read from unified headers:
//
//	anthropic-ratelimit-unified-5h-utilization: 0.19
//	anthropic-ratelimit-unified-5h-reset: 1778479200
//	anthropic-ratelimit-unified-5h-status: allowed
//	anthropic-ratelimit-unified-7d-utilization: 0.45
//	anthropic-ratelimit-unified-7d-reset: 1778814000
//	anthropic-ratelimit-unified-7d-status: allowed
//
// The usage request does not run inference or consume model tokens.

const (
	// anthropicAPIBaseURL is the production API origin.
	anthropicAPIBaseURL = "https://api.anthropic.com"

	// oauthBetaHeader is the beta flag Claude Code passes when
	// authenticating with an OAuth bearer rather than an API key.
	// Sourced from Claude Code's `src/constants/oauth.ts`.
	oauthBetaHeader = "oauth-2025-04-20"

	// anthropicVersion is the API version date string. Matches what
	// Claude Code's installed binary sends.
	anthropicVersion = "2023-06-01"

	// rateLimitProbeTimeout caps the HTTP call. Generous because the
	// probe runs on a background ticker — a slow network or a slow
	// server should produce a late ring update, not a failed one — but
	// a hung connection also shouldn't park a goroutine indefinitely.
	rateLimitProbeTimeout = 30 * time.Second
)

const maxCredentialFileBytes = 16 << 20

// claudeRateLimitHeaderPrefix is the common prefix for the response
// headers we care about. Listed as a single constant so the test
// fixtures and the parser stay aligned.
const claudeRateLimitHeaderPrefix = "Anthropic-Ratelimit-Unified-"

// ratelimitWindows maps legacy response-header segments to the corresponding
// Claude wire identifier. Canonical identity, label, and duration come from
// rateLimitDescriptorForType so all ingestion paths stay aligned.
var ratelimitWindows = []struct {
	headerSegment string
	rateLimitType string
}{
	{headerSegment: "5h", rateLimitType: "five_hour"},
	{headerSegment: "7d", rateLimitType: "seven_day"},
}

// CredentialPathForHome returns `<home>/.claude/.credentials.json`.
//
// The home is INJECTED, never resolved here. This probe reads a live
// bearer token and sends it to api.anthropic.com; an isolated boot
// (--harness / --soak, including AO_HARNESS_KEEP_HOME) must be unable to
// pick up the developer's real login, so the app layer's one provider-home
// seam decides which home this is.
func CredentialPathForHome(home string) string {
	return filepath.Join(home, ".claude", ".credentials.json")
}

// ProbeRateLimits reads Claude's OAuth usage endpoint for the login stored
// under home and returns every advertised account limit.
//
// Returns ErrNoCredentials when `<home>/.claude/.credentials.json` is
// missing (user hasn't run `claude login` yet) — callers should treat this
// as "probe not available" rather than a hard error.
//
// Returns a wrapped error for transport failures, non-2xx responses,
// or an unusable response. Errors
// are logged at the app layer; the global rate-limit store keeps its
// last-known value rather than wiping on a transient failure.
func ProbeRateLimits(ctx context.Context, httpClient *http.Client, home string) (provider.RateLimitsSnapshot, error) {
	if strings.TrimSpace(home) == "" {
		return provider.RateLimitsSnapshot{}, fmt.Errorf("claude: rate-limit probe: empty provider home")
	}
	return ProbeRateLimitsFromCredentialPath(ctx, httpClient, CredentialPathForHome(home))
}

// ProbeRateLimitsFromCredentialPath reads one native credential file.
// Credential contents stay local and are used only as the bearer on the HTTPS
// request.
func ProbeRateLimitsFromCredentialPath(ctx context.Context, httpClient *http.Client, credentialPath string) (provider.RateLimitsSnapshot, error) {
	data, err := readCredentialFile(credentialPath)
	if err != nil {
		return provider.RateLimitsSnapshot{}, err
	}
	return ProbeRateLimitsFromCredentialData(ctx, httpClient, data)
}

// ProbeRateLimitsFromCredentialData supports provider-native secure stores
// such as macOS Keychain without materializing their contents in an
// Agent Overflow-owned file.
func ProbeRateLimitsFromCredentialData(ctx context.Context, httpClient *http.Client, data []byte) (provider.RateLimitsSnapshot, error) {
	bearer, err := loadOAuthBearer(data)
	if err != nil {
		return provider.RateLimitsSnapshot{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, rateLimitProbeTimeout)
	defer cancel()

	body, headers, err := executeUsageProbe(probeCtx, httpClient, bearer)
	if err != nil {
		return provider.RateLimitsSnapshot{}, err
	}
	snapshot, parseErr := parseUsageResponse(body, time.Now())
	if parseErr == nil {
		return snapshot, nil
	}
	// Compatibility fallback for OAuth servers that do not expose the usage
	// endpoint but still return the classic unified headers.
	if legacy, headerErr := parseRateLimitsFromHeaders(headers, time.Now()); headerErr == nil {
		return legacy, nil
	}
	return provider.RateLimitsSnapshot{}, parseErr
}

// ErrNoCredentials signals that ~/.claude/.credentials.json is missing
// or unreadable. Distinct from a network/HTTP error so callers can log
// at debug level (expected when the user hasn't authenticated yet)
// rather than at error level (a real probe failure).
var ErrNoCredentials = errors.New("claude: oauth credentials not found")

// ErrOAuthUnauthorized lets the App ask the native CLI to run its normal
// refresh-token path before retrying the read-only usage request.
var ErrOAuthUnauthorized = errors.New("claude: oauth bearer expired or invalid")

// RateLimitedError is the usage endpoint's 429. RetryAfter carries the
// server's Retry-After when it sent a usable one, zero otherwise — callers
// pick their own default backoff for zero.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf(
			"claude: usage endpoint returned 429 Too Many Requests (retry after %s)",
			e.RetryAfter.Round(time.Second),
		)
	}
	return "claude: usage endpoint returned 429 Too Many Requests"
}

// retryAfterDuration parses an HTTP Retry-After value — delta-seconds or
// HTTP-date. Zero means absent or unusable.
func retryAfterDuration(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func readCredentialFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoCredentials
		}
		return nil, fmt.Errorf("claude: inspect credentials %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("claude: credential path is not a regular file")
	}
	if info.Size() > maxCredentialFileBytes {
		return nil, errors.New("claude: credential file exceeds size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("claude: open credentials: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("claude: inspect opened credentials: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, errors.New("claude: credential file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("claude: read credentials: %w", err)
	}
	if len(data) > maxCredentialFileBytes {
		return nil, errors.New("claude: credential file exceeds size limit")
	}
	return data, nil
}

func loadOAuthBearer(data []byte) (string, error) {
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

func executeUsageProbe(ctx context.Context, httpClient *http.Client, bearer string) ([]byte, http.Header, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicAPIBaseURL+"/api/oauth/usage", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("claude: build usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("anthropic-beta", oauthBetaHeader)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("claude: usage request: %w", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return nil, nil, fmt.Errorf("claude: read usage response: %w", readErr)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, nil, fmt.Errorf("claude: usage probe unauthorized: %w", ErrOAuthUnauthorized)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, nil, &RateLimitedError{
			RetryAfter: retryAfterDuration(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	if resp.StatusCode >= 400 {
		return nil, resp.Header, fmt.Errorf("claude: usage endpoint returned %s", resp.Status)
	}
	return body, resp.Header, nil
}

type usageLimit struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	ResetsAt *string `json:"resets_at"`
	IsActive bool    `json:"is_active"`
	Scope    struct {
		Model *struct {
			ID          *string `json:"id"`
			DisplayName string  `json:"display_name"`
		} `json:"model"`
		Surface *struct {
			ID          *string `json:"id"`
			DisplayName string  `json:"display_name"`
		} `json:"surface"`
	} `json:"scope"`
}

func parseUsageResponse(data []byte, now time.Time) (provider.RateLimitsSnapshot, error) {
	var payload struct {
		Limits []usageLimit `json:"limits"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return provider.RateLimitsSnapshot{}, fmt.Errorf("claude: parse usage response: %w", err)
	}
	if len(payload.Limits) == 0 {
		return provider.RateLimitsSnapshot{}, fmt.Errorf("claude: usage response missing limits")
	}

	entries := make([]provider.RateLimitEntry, 0, len(payload.Limits))
	seen := make(map[string]int, len(payload.Limits))
	// A limit this parser had to skip is a limit the snapshot cannot speak
	// for, so the reading stops being the server's whole answer and the
	// merge must not prune against it.
	skipped := false
	for _, limit := range payload.Limits {
		limitID := strings.TrimSpace(limit.Kind)
		if limitID == "" {
			skipped = true
			continue
		}
		limitName := usageLimitName(limit)
		if scopeKey := usageLimitScopeKey(limit); scopeKey != "" {
			limitID += ":" + scopeKey
		}
		baseLimitID := limitID
		if duplicate := seen[baseLimitID]; duplicate > 0 {
			limitID += ":" + strconv.Itoa(duplicate+1)
		}
		seen[baseLimitID]++

		resetsAt := int64(0)
		if limit.ResetsAt != nil && strings.TrimSpace(*limit.ResetsAt) != "" {
			parsed, err := time.Parse(time.RFC3339Nano, *limit.ResetsAt)
			if err != nil {
				skipped = true
				continue
			}
			resetsAt = parsed.Unix()
		}
		entries = append(entries, provider.RateLimitEntry{
			LimitID:     limitID,
			LimitName:   limitName,
			UsedPercent: limit.Percent,
			WindowMins:  usageLimitWindowMins(limit.Group),
			ResetsAt:    resetsAt,
		})
	}
	if len(entries) == 0 {
		return provider.RateLimitsSnapshot{}, fmt.Errorf("claude: usage response contained no usable limits")
	}
	return provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		Limits:    entries,
		UpdatedAt: now.UnixMilli(),
		Complete:  !skipped,
	}, nil
}

func usageLimitWindowMins(group string) int {
	switch strings.ToLower(strings.TrimSpace(group)) {
	case "session":
		return 5 * 60
	case "weekly":
		return 7 * 24 * 60
	case "daily":
		return 24 * 60
	case "monthly":
		return 30 * 24 * 60
	default:
		return 0
	}
}

func usageLimitName(limit usageLimit) string {
	if limit.Scope.Model != nil && strings.TrimSpace(limit.Scope.Model.DisplayName) != "" {
		return strings.TrimSpace(limit.Scope.Model.DisplayName)
	}
	if limit.Scope.Surface != nil && strings.TrimSpace(limit.Scope.Surface.DisplayName) != "" {
		return strings.TrimSpace(limit.Scope.Surface.DisplayName)
	}
	switch limit.Kind {
	case "session":
		return "Current session"
	case "weekly_all":
		return "All models"
	case "weekly_scoped":
		return "Scoped weekly"
	default:
		return strings.ReplaceAll(limit.Kind, "_", " ")
	}
}

func usageLimitScopeKey(limit usageLimit) string {
	var value string
	switch {
	case limit.Scope.Model != nil && limit.Scope.Model.ID != nil:
		value = *limit.Scope.Model.ID
	case limit.Scope.Model != nil:
		value = limit.Scope.Model.DisplayName
	case limit.Scope.Surface != nil && limit.Scope.Surface.ID != nil:
		value = *limit.Scope.Surface.ID
	case limit.Scope.Surface != nil:
		value = limit.Scope.Surface.DisplayName
	}
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, value)
	return strings.Trim(value, "-")
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
//
// The result is deliberately NOT Complete: these headers carry the two
// unified windows and nothing else, so a model-scoped weekly bucket is
// invisible here and must keep whatever the last full reading said.
func parseRateLimitsFromHeaders(headers http.Header, now time.Time) (provider.RateLimitsSnapshot, error) {
	entries := make([]provider.RateLimitEntry, 0, len(ratelimitWindows))
	for _, w := range ratelimitWindows {
		descriptor, known := rateLimitDescriptorForType(w.rateLimitType)
		if !known {
			continue
		}
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
			LimitID:     descriptor.limitID,
			LimitName:   descriptor.limitName,
			UsedPercent: util * 100,
			WindowMins:  descriptor.windowMins,
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
