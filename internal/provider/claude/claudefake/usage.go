package claudefake

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider/claude"
)

// UsageLimit is one entry of a fake usage answer, in the endpoint's terms.
type UsageLimit struct {
	// Kind is the endpoint's limit kind: session, weekly_all, weekly_scoped.
	Kind string `json:"kind"`
	// Group is the window: session, daily, weekly or monthly.
	Group   string  `json:"group"`
	Percent float64 `json:"percent"`
	// ResetsAt is RFC 3339; empty reports no reset time.
	ResetsAt string `json:"resets_at,omitempty"`
	// Model is the display name of the model a scoped limit covers.
	Model string `json:"model,omitempty"`
}

// Usage holds the usage answers for the identities a test sets. The usage
// endpoint is answered only for an identity with an answer set; every other
// request is left to the caller, which refuses it as before.
type Usage struct {
	mu       sync.Mutex
	readings map[string][]UsageLimit
}

func NewUsage() *Usage {
	return &Usage{readings: make(map[string][]UsageLimit)}
}

// Set makes the usage endpoint answer limits for the token of email.
func (u *Usage) Set(email string, limits []UsageLimit) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return errors.New("claudefake: usage needs an email")
	}
	if len(limits) == 0 {
		return errors.New("claudefake: usage needs at least one limit; clear it instead")
	}
	for i, limit := range limits {
		if strings.TrimSpace(limit.Kind) == "" {
			return fmt.Errorf("claudefake: usage limit %d has no kind", i)
		}
		if limit.ResetsAt != "" {
			if _, err := time.Parse(time.RFC3339Nano, limit.ResetsAt); err != nil {
				return fmt.Errorf("claudefake: usage limit %d resets_at: %w", i, err)
			}
		}
	}
	u.mu.Lock()
	u.readings[email] = append([]UsageLimit(nil), limits...)
	u.mu.Unlock()
	return nil
}

// Clear removes the answer for email, or every answer when email is empty.
func (u *Usage) Clear(email string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if email = strings.TrimSpace(email); email == "" {
		clear(u.readings)
		return
	}
	delete(u.readings, email)
}

// IsUsage reports whether req is a usage request.
func IsUsage(req *http.Request) bool {
	return req.Method == http.MethodGet && req.URL.String() == claude.UsageURL
}

// Serve answers a usage request whose bearer names an identity with an answer
// set. ok is false when it does not, and the caller answers the request.
func (u *Usage) Serve(req *http.Request) (resp *http.Response, ok bool, err error) {
	if u == nil || !IsUsage(req) {
		return nil, false, nil
	}
	email, _, identified := bearerIdentity(req)
	if !identified {
		return nil, false, nil
	}
	u.mu.Lock()
	limits, found := u.readings[email]
	u.mu.Unlock()
	if !found {
		return nil, false, nil
	}
	type model struct {
		DisplayName string `json:"display_name"`
	}
	type scope struct {
		Model *model `json:"model"`
	}
	type limit struct {
		Kind     string  `json:"kind"`
		Group    string  `json:"group"`
		Percent  float64 `json:"percent"`
		ResetsAt *string `json:"resets_at"`
		IsActive bool    `json:"is_active"`
		Scope    *scope  `json:"scope"`
	}
	payload := struct {
		Limits []limit `json:"limits"`
	}{Limits: make([]limit, 0, len(limits))}
	for _, entry := range limits {
		wire := limit{Kind: entry.Kind, Group: entry.Group, Percent: entry.Percent, IsActive: true}
		if entry.ResetsAt != "" {
			resetsAt := entry.ResetsAt
			wire.ResetsAt = &resetsAt
		}
		if entry.Model != "" {
			wire.Scope = &scope{Model: &model{DisplayName: entry.Model}}
		}
		payload.Limits = append(payload.Limits, wire)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, true, fmt.Errorf("claudefake: encode usage: %w", err)
	}
	return response(req, http.StatusOK, string(body)), true, nil
}
