package jira

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// dialect reads Jira's conventions for the transport.
//
// Error bodies are {"errorMessages": [...], "errors": {"field": "message"}}
// on the platform API; some endpoints answer {"message": ...} or
// {"errorMessage": ...} instead. A refused login is marked by the
// X-Seraph-LoginReason header, which can arrive on a 200. A rate limit is
// always a 429 carrying Retry-After, and X-RateLimit-Reset (an ISO 8601
// time) on the burst limits
// (https://developer.atlassian.com/cloud/jira/platform/rate-limiting/).
type dialect struct{}

// refusedLogins are the X-Seraph-LoginReason values that mean the
// credential was refused. AUTHORISATION_FAILED is a signed-in user
// without permission, not a refused credential.
var refusedLogins = []string{"AUTHENTICATED_FAILED", "AUTHENTICATION_DENIED"}

func (dialect) RejectedLogin(h http.Header) bool {
	for _, v := range h.Values("X-Seraph-LoginReason") {
		for part := range strings.SplitSeq(v, ",") {
			if slices.Contains(refusedLogins, strings.TrimSpace(part)) {
				return true
			}
		}
	}
	return false
}

func (dialect) Problem(_ int, body []byte) trackerapi.Problem {
	var v struct {
		ErrorMessages []json.RawMessage `json:"errorMessages"`
		Errors        json.RawMessage   `json:"errors"`
		Message       string            `json:"message"`
		ErrorMessage  string            `json:"errorMessage"`
	}
	if json.Unmarshal(body, &v) != nil {
		return trackerapi.Problem{}
	}
	var messages []string
	for _, m := range v.ErrorMessages {
		if s := jsonText(m); s != "" {
			messages = append(messages, s)
		}
	}
	p := trackerapi.Problem{}
	var byField map[string]json.RawMessage
	var list []json.RawMessage
	switch {
	case json.Unmarshal(v.Errors, &byField) == nil && len(byField) > 0:
		p.Fields = make(map[string]string, len(byField))
		for field, msg := range byField {
			p.Fields[field] = jsonText(msg)
		}
	case json.Unmarshal(v.Errors, &list) == nil:
		for _, item := range list {
			if s := listedMessage(item); s != "" {
				messages = append(messages, s)
			}
		}
	}
	if len(messages) == 0 {
		messages = append(messages, firstNonEmpty(v.Message, v.ErrorMessage))
	}
	p.Message = strings.TrimSpace(strings.Join(messages, "; "))
	return p
}

// listedMessage reads one entry of an "errors" list: a string, or an
// object with a message, title or detail.
func listedMessage(raw json.RawMessage) string {
	var obj struct {
		Message string `json:"message"`
		Title   string `json:"title"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return firstNonEmpty(obj.Message, obj.Title, obj.Detail)
	}
	return jsonText(raw)
}

// jsonText is a JSON string's text, or the compact JSON of anything else.
func jsonText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(raw))
}

// RateLimited marks a 429 and, when the answer names no Retry-After,
// takes X-RateLimit-Reset as the time it lifts. Jira marks no limit on
// any other status.
func (dialect) RateLimited(status int, h http.Header, _ []byte) (time.Time, bool) {
	if status != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	if h.Get("Retry-After") != "" {
		return time.Time{}, true
	}
	if t, err := time.Parse(time.RFC3339, h.Get("X-RateLimit-Reset")); err == nil {
		return t, true
	}
	return time.Time{}, true
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
