package codex

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// codexRetryCountsRE captures the first "N/M" attempt/total pair embedded
// in a Codex retry message. Both numbers are bounded to 5 digits so a stray
// long digit run can't be parsed as a retry count; non-digit boundaries
// stop us from picking up substrings of a longer number ("12345/67890" no
// longer matches as "1234/6789", which would be misleading).
var codexRetryCountsRE = regexp.MustCompile(`(?:^|[^0-9])(\d{1,5})/(\d{1,5})(?:[^0-9]|$)`)

// ClassifyNotification converts a Codex app-server notification into zero or
// more ProviderEvents. Parent/child linkage for CollabAgent children is
// resolved by the Session dispatch path, not here: the protocol only gives us
// child provider-thread IDs, so the session owns the receiver-thread ->
// parent-card mapping and stamps ParentToolUseID onto routed child events.
//
// The method catalog is grouped into per-family dispatchers (turn / item /
// thread / account / etc.) so each family's shape lives in one place. Each
// dispatcher returns (events, handled): when handled is false the caller
// falls through to the next group. Adding a new method means editing the
// dispatcher for its family, not the top-level switch.
func ClassifyNotification(threadID, method string, params json.RawMessage) []provider.ProviderEvent {
	now := time.Now()

	if events, ok := classifyTurnNotification(threadID, method, params, now); ok {
		return events
	}
	if events, ok := classifyItemNotification(threadID, method, params, now); ok {
		return events
	}
	if events, ok := classifyRawResponseNotification(threadID, method, params, now); ok {
		return events
	}
	if events, ok := classifyThreadNotification(threadID, method, params, now); ok {
		return events
	}
	if events, ok := classifyAccountNotification(threadID, method, params, now); ok {
		return events
	}
	if events, ok := classifyHookNotification(threadID, method, params, now); ok {
		return events
	}
	if events, ok := classifyMiscNotification(threadID, method, params, now); ok {
		return events
	}
	return nil
}

func classifyHookNotification(threadID, method string, params json.RawMessage, now time.Time) ([]provider.ProviderEvent, bool) {
	switch method {
	case "hook/started":
		return []provider.ProviderEvent{codexNotificationEvent(threadID, "hook", hookSummary(params, "started"), params, now)}, true
	case "hook/completed":
		return []provider.ProviderEvent{codexNotificationEvent(threadID, "hook", hookSummary(params, "completed"), params, now)}, true
	}
	return nil, false
}

func hookSummary(params json.RawMessage, fallbackStatus string) string {
	eventName := readNestedString(params, "run", "eventName")
	status := readNestedString(params, "run", "status")
	if status == "" {
		status = fallbackStatus
	}
	if eventName == "" {
		return "Hook " + status
	}
	return eventName + " hook (" + status + ")"
}

// classifyMiscNotification handles the remaining grab-bag: errors, server
// request resolution, and pure-informational notices.
func classifyMiscNotification(threadID, method string, params json.RawMessage, now time.Time) ([]provider.ProviderEvent, bool) {
	switch method {
	case "error":
		errorMsg := readNestedString(params, "error", "message")
		willRetry := readTopLevelBool(params, "willRetry")
		if willRetry {
			// Codex doesn't structure attempt/max separately; the count
			// is embedded in the message text ("Reconnecting... 2/5").
			// Try to parse it for parity with Claude's `system.api_retry`
			// shape (which carries `attempt`/`max_retries` directly).
			// Failed parse leaves attempt=0 — triage's hide-first-three
			// rule treats unknown counts as "show" so a one-off Codex
			// reconnect still surfaces. Don't over-classify.
			attempt, maxRetries := parseCodexRetryCounts(errorMsg)
			meta, _ := json.Marshal(map[string]any{
				"attempt":     attempt,
				"max_retries": maxRetries,
				"error":       errorMsg,
				"wire":        json.RawMessage(params),
			})
			return []provider.ProviderEvent{{
				Kind:      provider.EventAPIRetry,
				ThreadID:  threadID,
				Meta:      meta,
				Timestamp: now,
			}}, true
		}
		// willRetry:false — fatal. Tag meta.fatal so the triage router's
		// fatal branch fires (closes the open turn, synthesizes
		// TurnComplete since the subprocess EOF will follow).
		fatalMeta, _ := json.Marshal(map[string]any{
			"fatal": true,
			"error": errorMsg,
			"wire":  json.RawMessage(params),
		})
		return []provider.ProviderEvent{{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			Content:   errorMsg,
			Meta:      fatalMeta,
			Timestamp: now,
		}}, true

	case "serverRequest/resolved":
		requestID := readTopLevelIDString(params, "providerRequestId")
		if requestID == "" {
			requestID = readTopLevelIDString(params, "requestId")
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventApprovalResolved,
			ThreadID:  threadID,
			ItemID:    requestID,
			Meta:      params,
			Timestamp: now,
		}}, true

	case "warning":
		return []provider.ProviderEvent{codexNotificationEvent(threadID, "warning", readTopLevelString(params, "message"), params, now)}, true

	case "guardianWarning":
		return []provider.ProviderEvent{codexNotificationEvent(threadID, "warning", readTopLevelString(params, "message"), params, now)}, true

	case "configWarning":
		summary := readTopLevelString(params, "summary")
		if details := readTopLevelString(params, "details"); details != "" {
			summary = strings.TrimSpace(summary + ": " + details)
		}
		return []provider.ProviderEvent{codexNotificationEvent(threadID, "warning", summary, params, now)}, true

	case "deprecationNotice":
		summary := readTopLevelString(params, "summary")
		if details := readTopLevelString(params, "details"); details != "" {
			summary = strings.TrimSpace(summary + ": " + details)
		}
		return []provider.ProviderEvent{codexNotificationEvent(threadID, "deprecation_notice", summary, params, now)}, true
	}
	return nil, false
}

func codexNotificationEvent(threadID, kind, summary string, params json.RawMessage, now time.Time) provider.ProviderEvent {
	if strings.TrimSpace(summary) == "" {
		summary = "Provider notification"
	}
	return provider.ProviderEvent{
		Kind:      provider.EventNotification,
		ThreadID:  threadID,
		Content:   summary,
		Meta:      mergeMetaKeys(params, map[string]any{"kind": kind, "title": summary}),
		Timestamp: now,
	}
}

func reviewStatusSummary(prefix, review string) string {
	review = strings.TrimSpace(review)
	if review == "" {
		return prefix
	}
	return prefix + ": " + review
}
