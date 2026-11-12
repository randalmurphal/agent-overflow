package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/logging"
)

// ProfileURL is the endpoint Claude Code itself reads to learn who an OAuth
// access token belongs to; it fills `oauthAccount` from this answer. Unlike
// that record, which sits in a shared config file any running CLI may write
// back, the answer is bound to the token it was asked with, so it is the only
// identity that can be paired with a credential.
const ProfileURL = anthropicAPIBaseURL + "/api/oauth/profile"

// profileScope is the OAuth scope the endpoint requires. Claude Code checks
// the credential's `scopes` for it before asking, and so does this client.
const profileScope = "user:profile"

const profileTimeout = 10 * time.Second

// ErrNoProfileScope reports a credential that cannot be identified: it was
// issued without the scope the profile endpoint requires.
var ErrNoProfileScope = errors.New("claude: credential lacks the user:profile scope")

// Profile is who the Anthropic API says an access token belongs to.
type Profile struct {
	Email   string
	OrgID   string
	OrgName string
}

// ProbeProfile asks the API who the given native credential belongs to. It
// reads only the access token and scopes; the credential is never written.
//
// Returns ErrNoCredentials when the bytes carry no access token,
// ErrNoProfileScope when the token cannot be identified, and
// ErrOAuthUnauthorized when the server rejects it.
func ProbeProfile(ctx context.Context, httpClient *http.Client, credential []byte) (Profile, error) {
	var creds struct {
		ClaudeAIOauth struct {
			AccessToken string   `json:"accessToken"`
			Scopes      []string `json:"scopes"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(credential, &creds); err != nil {
		return Profile{}, fmt.Errorf("claude: parse credentials: %w", err)
	}
	bearer := strings.TrimSpace(creds.ClaudeAIOauth.AccessToken)
	if bearer == "" {
		return Profile{}, ErrNoCredentials
	}
	if !slices.Contains(creds.ClaudeAIOauth.Scopes, profileScope) {
		return Profile{}, ErrNoProfileScope
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, profileTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ProfileURL, nil)
	if err != nil {
		return Profile{}, fmt.Errorf("claude: build profile request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := httpClient.Do(logging.TraceHTTPRequest(req, "claude-profile"))
	if err != nil {
		return Profile{}, fmt.Errorf("claude: profile request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Profile{}, fmt.Errorf("claude: read profile response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return Profile{}, fmt.Errorf("claude: profile request unauthorized: %w", ErrOAuthUnauthorized)
	}
	if resp.StatusCode >= 400 {
		return Profile{}, fmt.Errorf("claude: profile endpoint returned %s", resp.Status)
	}
	return parseProfileResponse(body)
}

// parseProfileResponse requires the two fields Claude Code itself validates
// (account.email and organization.uuid); the organization name is optional.
func parseProfileResponse(body []byte) (Profile, error) {
	var payload struct {
		Account struct {
			Email string `json:"email"`
		} `json:"account"`
		Organization struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Profile{}, fmt.Errorf("claude: parse profile response: %w", err)
	}
	profile := Profile{
		Email:   strings.TrimSpace(payload.Account.Email),
		OrgID:   strings.TrimSpace(payload.Organization.UUID),
		OrgName: strings.TrimSpace(payload.Organization.Name),
	}
	if profile.Email == "" || profile.OrgID == "" {
		return Profile{}, errors.New("claude: profile response is missing the account email or organization uuid")
	}
	return profile, nil
}
