package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

const (
	dailyCloudCodeEndpoint = "https://daily-cloudcode-pa.googleapis.com"
	prodCloudCodeEndpoint  = "https://cloudcode-pa.googleapis.com"
	maxQuotaResponseBytes  = 512 * 1024
)

// RateLimitedError carries the server's Retry-After hint when Google rate-limits
// a quota poll (HTTP 429). Implements usagebackoff.RetryAfter().
type RateLimitedError struct {
	RetryAfterDuration time.Duration
	Message            string
}

func (e RateLimitedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "Antigravity usage endpoint rate limited this account (HTTP 429)"
}

func (e RateLimitedError) RetryAfter() time.Duration {
	return e.RetryAfterDuration
}

// UserAgent returns the CloudCode API user-agent header expected by
// Antigravity's quota endpoints.
func UserAgent() string {
	return fmt.Sprintf("antigravity/hub/2.24.0 (aidev_client; os_type=%s; arch=%s)", runtime.GOOS, runtime.GOARCH)
}

// ProbeRateLimitsFromCredentialData fetches the account's live 5-hour and
// weekly quota buckets from Antigravity's CloudCode quota summary endpoint
// (falling back to fetchAvailableModels for older accounts).
func ProbeRateLimitsFromCredentialData(
	ctx context.Context,
	client *http.Client,
	credentialData []byte,
) (provider.RateLimitsSnapshot, error) {
	return probeRateLimitsAtEndpoints(
		ctx,
		client,
		[]string{dailyCloudCodeEndpoint, prodCloudCodeEndpoint},
		credentialData,
		time.Now(),
	)
}

func probeRateLimitsAtEndpoints(
	ctx context.Context,
	client *http.Client,
	endpoints []string,
	credentialData []byte,
	now time.Time,
) (provider.RateLimitsSnapshot, error) {
	cred, err := ParseCredential(credentialData)
	if err != nil {
		return provider.RateLimitsSnapshot{}, err
	}
	if cred.AccessToken == "" {
		return provider.RateLimitsSnapshot{}, ErrNoCredentials
	}
	if client == nil {
		client = http.DefaultClient
	}

	var lastErr error
	sawUnauthorized := false
	for _, base := range endpoints {
		trimmedBase := strings.TrimRight(base, "/")
		body, err := postQuotaJSON(ctx, client, trimmedBase+"/v1internal:retrieveUserQuotaSummary", cred.AccessToken)
		var parseErr error
		if err != nil {
			var rl RateLimitedError
			if errors.As(err, &rl) {
				return provider.RateLimitsSnapshot{}, err
			}
			if errors.Is(err, ErrOAuthUnauthorized) {
				// An enterprise/Workspace token may receive 401/403 on the
				// consumer endpoint (daily-cloudcode-pa) while succeeding on
				// cloudcode-pa; try remaining endpoints before declaring auth
				// invalid.
				sawUnauthorized = true
				lastErr = err
				continue
			}
			lastErr = err
		} else {
			limits, pErr := parseQuotaSummary(body, now)
			if pErr == nil && len(limits) > 0 {
				return provider.RateLimitsSnapshot{
					Provider:  string(provider.Agy),
					Limits:    limits,
					UpdatedAt: now.UnixMilli(),
					Complete:  true,
				}, nil
			}
			parseErr = pErr
		}
		// Fallback for accounts whose retrieveUserQuotaSummary is unsupported
		// or returns empty buckets.
		modelsBody, mErr := postQuotaJSON(ctx, client, trimmedBase+"/v1internal:fetchAvailableModels", cred.AccessToken)
		if mErr != nil {
			var rl RateLimitedError
			if errors.As(mErr, &rl) {
				return provider.RateLimitsSnapshot{}, mErr
			}
			if errors.Is(mErr, ErrOAuthUnauthorized) {
				sawUnauthorized = true
			}
			lastErr = mErr
			continue
		}
		modelLimits, mParseErr := parseAvailableModelsQuota(modelsBody)
		if mParseErr == nil && len(modelLimits) > 0 {
			return provider.RateLimitsSnapshot{
				Provider:  string(provider.Agy),
				Limits:    modelLimits,
				UpdatedAt: now.UnixMilli(),
				Complete:  true,
			}, nil
		}
		if parseErr != nil {
			lastErr = parseErr
		} else if mParseErr != nil {
			lastErr = mParseErr
		}
	}
	if sawUnauthorized {
		return provider.RateLimitsSnapshot{}, ErrOAuthUnauthorized
	}
	if lastErr != nil {
		return provider.RateLimitsSnapshot{}, lastErr
	}
	return provider.RateLimitsSnapshot{}, errors.New("agy: quota endpoint returned no usage buckets")
}

func postQuotaJSON(ctx context.Context, client *http.Client, endpoint, accessToken string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agy: quota request %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxQuotaResponseBytes+1))
	if readErr != nil {
		return nil, fmt.Errorf("agy: read quota response: %w", readErr)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrOAuthUnauthorized
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, RateLimitedError{
			RetryAfterDuration: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("agy: quota endpoint returned HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(header); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

type quotaBucket struct {
	BucketID          string   `json:"bucketId"`
	Window            string   `json:"window"`
	DisplayName       string   `json:"displayName"`
	Description       string   `json:"description"`
	Disabled          bool     `json:"disabled"`
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
}

func parseQuotaSummary(body []byte, now time.Time) ([]provider.RateLimitEntry, error) {
	var summary struct {
		Groups []struct {
			DisplayName string        `json:"displayName"`
			Buckets     []quotaBucket `json:"buckets"`
		} `json:"groups"`
		Buckets []quotaBucket `json:"buckets"`
	}
	if err := json.Unmarshal(body, &summary); err != nil {
		return nil, fmt.Errorf("agy: decode quota summary: %w", err)
	}

	type groupBuckets struct {
		displayName string
		buckets     []quotaBucket
	}
	var groups []groupBuckets
	hasGrouped := false
	for _, g := range summary.Groups {
		if len(g.Buckets) > 0 {
			hasGrouped = true
			groups = append(groups, groupBuckets{displayName: strings.TrimSpace(g.DisplayName), buckets: g.Buckets})
		}
	}
	if !hasGrouped && len(summary.Buckets) > 0 {
		groups = []groupBuckets{{buckets: summary.Buckets}}
	}

	byID := make(map[string]provider.RateLimitEntry)
	var order []string
	for _, g := range groups {
		for _, b := range g.buckets {
			if b.Disabled || strings.TrimSpace(b.BucketID) == "" || b.RemainingFraction == nil || math.IsNaN(*b.RemainingFraction) || math.IsInf(*b.RemainingFraction, 0) {
				continue
			}
			family := bucketFamilyName(b.BucketID, g.displayName)
			windowMins := 0
			windowLabel := strings.TrimSpace(b.DisplayName)
			switch b.Window {
			case "5h":
				windowMins = 5 * 60
				windowLabel = "5-hour"
			case "weekly":
				windowMins = 7 * 24 * 60
				windowLabel = "Weekly"
			default:
				if windowLabel == "" {
					windowLabel = "Quota"
				}
			}

			rem := math.Max(0, math.Min(1, *b.RemainingFraction))
			usedPct := math.Round((1-rem)*1000) / 10

			var resetsAt int64
			if ts, ok := parseResetTimestamp(b.ResetTime); ok {
				// Untouched full-window rolling reset matches now + windowMins within 60s.
				untouched := rem == 1 && b.Description == "" && windowMins > 0 &&
					math.Abs(ts.Sub(now).Seconds()-float64(windowMins*60)) <= 60
				if !untouched {
					resetsAt = ts.Unix()
				}
			}

			entry := provider.RateLimitEntry{
				LimitID:     b.BucketID,
				LimitName:   family + " · " + windowLabel,
				UsedPercent: usedPct,
				WindowMins:  windowMins,
				ResetsAt:    resetsAt,
			}
			if _, exists := byID[entry.LimitID]; !exists {
				order = append(order, entry.LimitID)
			}
			byID[entry.LimitID] = entry
		}
	}

	out := make([]provider.RateLimitEntry, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}

func parseAvailableModelsQuota(body []byte) ([]provider.RateLimitEntry, error) {
	type modelQuota struct {
		RemainingFraction *float64 `json:"remainingFraction"`
		ResetTime         string   `json:"resetTime"`
	}
	var payload struct {
		Models map[string]struct {
			ModelProvider   string      `json:"modelProvider"`
			QuotaInfo       *modelQuota `json:"quotaInfo"`
			WeeklyQuotaInfo *modelQuota `json:"weeklyQuotaInfo"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("agy: decode fetchAvailableModels: %w", err)
	}

	byID := make(map[string]provider.RateLimitEntry)
	for modelID, model := range payload.Models {
		family := ""
		switch {
		case model.ModelProvider == "MODEL_PROVIDER_GOOGLE" || strings.HasPrefix(modelID, "gemini-"):
			family = "Gemini"
		case model.ModelProvider == "MODEL_PROVIDER_ANTHROPIC" || strings.HasPrefix(modelID, "claude-"):
			family = "Claude"
		case model.ModelProvider == "MODEL_PROVIDER_OPENAI" || strings.HasPrefix(modelID, "gpt-"):
			family = "GPT"
		default:
			continue
		}
		for _, pair := range []struct {
			suffix string
			label  string
			mins   int
			quota  *modelQuota
		}{
			{"quota", "5-hour", 5 * 60, model.QuotaInfo},
			{"weekly", "Weekly", 7 * 24 * 60, model.WeeklyQuotaInfo},
		} {
			if pair.quota == nil || pair.quota.RemainingFraction == nil {
				continue
			}
			rem := math.Max(0, math.Min(1, *pair.quota.RemainingFraction))
			usedPct := math.Round((1-rem)*1000) / 10
			var resetsAt int64
			if ts, ok := parseResetTimestamp(pair.quota.ResetTime); ok {
				resetsAt = ts.Unix()
			}
			id := strings.ToLower(family) + "-" + pair.suffix
			entry := provider.RateLimitEntry{
				LimitID:     id,
				LimitName:   family + " · " + pair.label,
				UsedPercent: usedPct,
				WindowMins:  pair.mins,
				ResetsAt:    resetsAt,
			}
			if prev, exists := byID[id]; !exists || entry.UsedPercent > prev.UsedPercent {
				byID[id] = entry
			}
		}
	}

	keys := make([]string, 0, len(byID))
	for k := range byID {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]provider.RateLimitEntry, 0, len(keys))
	for _, k := range keys {
		out = append(out, byID[k])
	}
	return out, nil
}

func bucketFamilyName(bucketID, groupDisplayName string) string {
	switch {
	case strings.HasPrefix(bucketID, "gemini-argon-"):
		if groupDisplayName != "" {
			return groupDisplayName
		}
		return "Gemini Argon"
	case strings.HasPrefix(bucketID, "gemini-carbon-"):
		if groupDisplayName != "" {
			return groupDisplayName
		}
		return "Gemini Carbon"
	case strings.HasPrefix(bucketID, "gemini-"):
		return "Gemini"
	case strings.HasPrefix(bucketID, "3p-"):
		return "Claude & GPT (shared)"
	default:
		if groupDisplayName != "" {
			return groupDisplayName
		}
		return bucketID
	}
}

func parseResetTimestamp(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	return time.Time{}, false
}
