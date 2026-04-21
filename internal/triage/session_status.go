package triage

import (
	"encoding/json"
	"log"
	"strings"

	"agent-overflow/internal/provider"
)

// classifySessionStatus maps a provider-specific EventSessionStatus to a
// persistent ProviderStatusEventKind + a banner Message. Returns
// (kind, message, known, persistent): kind and message are meaningful
// only when both known and persistent are true.
//
// The Content string is the coarse discriminator ("retrying", "ok",
// "disconnected", ...). For "retrying" we dig into Meta to pick the
// most-specific kind so the banner tells the user whether the retry is
// rate-limit, auth, or generic transient.
func classifySessionStatus(content string, meta json.RawMessage) (provider.ProviderStatusEventKind, string, bool, bool) {
	switch content {
	case "retrying":
		kind, reason := classifyRetryReason(meta)
		message := retryBannerMessage(reason)
		return kind, message, true, true
	case "ok", "ready":
		// Either vocabulary clears the banner. "ready" is what the
		// legacy detect flow uses; accept it so a migration in Agent 2's
		// adapter rewrite doesn't have to touch this file to flip a
		// pass-through kind.
		return provider.ProviderStatusOK, "", true, true
	case "disconnected", "session_state_changed", "error":
		// Transient — the working-indicator handles connection churn
		// and EventError handles terminal failures. Known but not
		// persistent.
		return "", "", true, false
	default:
		return "", "", false, false
	}
}

// classifyRetryReason extracts the upstream retry cause from the
// EventSessionStatus Meta payload and returns (kind, reason). The reason
// string is a short provider-reported token ("rate_limit", "server_error",
// an HTTP status, or an excerpt of an error message) that the banner
// surfaces verbatim in Message; the caller composes the final message.
//
// Sources:
//   - Claude api_retry: Meta has {"error":"rate_limit|authentication_failed|
//     server_error|invalid_request|billing_error|max_output_tokens|unknown",
//     "error_status":<HTTP>,"attempt":N,"max_retries":M,"retry_delay_ms":D}.
//   - Codex error+willRetry: Meta has {"willRetry":true,
//     "error":{"message":"..."}}.
//
// Empty / missing reason falls through to transient_retry with an empty
// reason, which the UI renders as "Retrying provider request...".
func classifyRetryReason(meta json.RawMessage) (provider.ProviderStatusEventKind, string) {
	if len(meta) == 0 {
		return provider.ProviderStatusTransientRetry, ""
	}

	// Claude shape: top-level string "error".
	if reason := readJSONString(metaAt(meta, "error")); reason != "" {
		return retryKindForReason(reason), reason
	}
	// Codex shape: nested error.message.
	if nested := strings.TrimSpace(metaNestedString(meta, "error", "message")); nested != "" {
		return retryKindForReason(nested), nested
	}
	// Legacy shape some adapters used: top-level "reason".
	if reason := readJSONString(metaAt(meta, "reason")); reason != "" {
		return retryKindForReason(reason), reason
	}
	return provider.ProviderStatusTransientRetry, ""
}

// claudeRetryErrorKinds maps Claude's closed-set `error` enum (carried
// on `system.api_retry`) to our banner status. Unlisted values fall
// through to transient_retry via the switch's default branch.
//
// Source: the Claude Agent SDK emits one of these exact strings in
// api_retry.data.error — keeping the switch keyed on the enum avoids
// the substring-match pitfalls (e.g. "401 requests/minute" would
// otherwise wrongly classify a rate_limit as unauthenticated).
var claudeRetryErrorKinds = map[string]provider.ProviderStatusEventKind{
	"rate_limit":            provider.ProviderStatusRateLimitedRetrying,
	"authentication_failed": provider.ProviderStatusUnauthenticated,
	"server_error":          provider.ProviderStatusTransientRetry,
	"invalid_request":       provider.ProviderStatusTransientRetry,
	"billing_error":         provider.ProviderStatusTransientRetry,
	"max_output_tokens":     provider.ProviderStatusTransientRetry,
	"unknown":               provider.ProviderStatusTransientRetry,
}

// retryKindForReason returns the ProviderStatusEventKind we want the
// banner to render for a given upstream reason. The reason can be a
// Claude SDK enum (closed set — matched exactly), a Codex error
// message (free-form — matched by signal phrase), or anything else a
// future provider surfaces.
//
// Order matters: we try the Claude enum first because it's an exact
// match and cannot collide with user-supplied text. Only when that
// fails do we fall back to substring matching on the free-form
// message, which we scope carefully so "401 requests/minute" doesn't
// classify a rate-limit retry as unauthenticated.
func retryKindForReason(reason string) provider.ProviderStatusEventKind {
	normalized := strings.ToLower(strings.TrimSpace(reason))
	if normalized == "" {
		return provider.ProviderStatusTransientRetry
	}
	// Exact match against Claude's closed enum.
	if kind, ok := claudeRetryErrorKinds[normalized]; ok {
		return kind
	}
	// Codex free-form messages + future-provider fallback. We check for
	// rate-limit signals FIRST because Codex rate-limit messages often
	// quote HTTP 429 next to the word "auth" ("authorization header:
	// ...") and we want rate_limit classification to win over a stray
	// "auth" fragment.
	//
	// The rate-limit probes use phrase boundaries ("rate limit",
	// "rate_limit") plus the HTTP-status token " 429 " / "429 " to
	// avoid false positives against unrelated messages that happen to
	// contain "429" as a substring of a number or path.
	if strings.Contains(normalized, "rate limit") ||
		strings.Contains(normalized, "rate_limit") ||
		strings.Contains(normalized, "quota") ||
		containsStatusCode(normalized, "429") {
		return provider.ProviderStatusRateLimitedRetrying
	}
	// Unauthenticated probes key on explicit auth phrasings — plain
	// "auth" alone is too weak (matches "authorization", "authored",
	// etc.). We require one of the canonical error tokens or an HTTP
	// 401/403 that's surfaced as its own word, not embedded in a
	// bigger number like "1401".
	if strings.Contains(normalized, "unauthenticated") ||
		strings.Contains(normalized, "unauthorized") ||
		strings.Contains(normalized, "authentication failed") ||
		strings.Contains(normalized, "authentication_failed") ||
		containsStatusCode(normalized, "401") ||
		containsStatusCode(normalized, "403") {
		return provider.ProviderStatusUnauthenticated
	}
	return provider.ProviderStatusTransientRetry
}

// containsStatusCode reports whether `haystack` contains the HTTP
// status code `code` as a standalone token — i.e. not a prefix or
// suffix of a larger digit run. Keeps "401 Unauthorized" matching
// while rejecting "1401 requests" from false-positive-classifying.
func containsStatusCode(haystack, code string) bool {
	i := strings.Index(haystack, code)
	for i != -1 {
		// boundary before
		if i > 0 {
			b := haystack[i-1]
			if b >= '0' && b <= '9' {
				haystack = haystack[i+len(code):]
				i = strings.Index(haystack, code)
				continue
			}
		}
		// boundary after
		end := i + len(code)
		if end < len(haystack) {
			b := haystack[end]
			if b >= '0' && b <= '9' {
				haystack = haystack[end:]
				i = strings.Index(haystack, code)
				continue
			}
		}
		return true
	}
	return false
}

// retryBannerMessage returns the banner Message field for a retry. The
// UI already renders kind-specific copy (see ProviderStatusBanner);
// Message is a human-readable detail line. We keep it short — Codex can
// send reason="Reconnecting... 2/5" and the UI clips to two lines.
func retryBannerMessage(reason string) string {
	if reason == "" {
		return "Retrying..."
	}
	return reason
}

// metaAt returns the raw JSON value at a top-level key, or nil if the
// key is absent or the meta is not a JSON object. Keeps the session
// status classifier independent of metaNestedString's "walk into nested
// objects" semantics.
func metaAt(meta json.RawMessage, key string) json.RawMessage {
	if len(meta) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(meta, &m) != nil {
		return nil
	}
	return m[key]
}

// unknownSessionStatusCap bounds the throttle set so a long-running
// process that sees a stream of novel session-status strings — a
// provider regression, a wire-format drift, or a fuzzed input — cannot
// grow the map without bound. When the cap is hit the oldest entries
// are dropped wholesale (map reset) which re-admits one extra log line
// per distinct value after a cap rollover. That is acceptable because
// the cap is orders of magnitude higher than the known-good enumeration
// (~6 persistent kinds today).
const unknownSessionStatusCap = 256

func (r *Router) logUnknownSessionStatusOnce(content string) {
	key := strings.TrimSpace(content)
	r.mu.Lock()
	if _, seen := r.unknownSessionStatusLogged[key]; seen {
		r.mu.Unlock()
		return
	}
	if len(r.unknownSessionStatusLogged) >= unknownSessionStatusCap {
		r.unknownSessionStatusLogged = make(map[string]struct{})
	}
	r.unknownSessionStatusLogged[key] = struct{}{}
	r.mu.Unlock()
	log.Printf("triage: unknown session-status content %q — dropping", key)
}
