package agy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
)

const (
	googleUserInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"
	maxUserInfoBytes  = 64 * 1024
)

// ErrNoCredentials reports that no usable Antigravity OAuth token was found in
// the credential bytes.
var ErrNoCredentials = errors.New("agy: no OAuth credentials found")

// ErrOAuthUnauthorized reports that Google rejected the Antigravity OAuth
// access token with HTTP 401 or 403.
var ErrOAuthUnauthorized = errors.New("agy: OAuth access token rejected")

// Credential is the parsed OAuth token record stored by agy 1.5.x in
// `<appDataDir>/antigravity-oauth-token` or macOS Keychain (`gemini`/`antigravity`).
type Credential struct {
	Email        string
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
	HasExpiry    bool
}

// ParseCredential decodes agy's OAuth JSON bytes.
// Supports both the nested `{"token":{...}}` CLI format (including
// `id_token_email`) and a flat token object.
func ParseCredential(data []byte) (Credential, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return Credential{}, ErrNoCredentials
	}
	var nested struct {
		IDTokenEmail string `json:"id_token_email"`
		Email        string `json:"email"`
		Token        *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Expiry       string `json:"expiry"`
		} `json:"token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Expiry       string `json:"expiry"`
	}
	if err := json.Unmarshal(data, &nested); err != nil {
		return Credential{}, fmt.Errorf("agy: parse OAuth credential: %w", err)
	}
	var access, refresh, expiryStr string
	if nested.Token != nil {
		access = strings.TrimSpace(nested.Token.AccessToken)
		refresh = strings.TrimSpace(nested.Token.RefreshToken)
		expiryStr = strings.TrimSpace(nested.Token.Expiry)
	} else {
		access = strings.TrimSpace(nested.AccessToken)
		refresh = strings.TrimSpace(nested.RefreshToken)
		expiryStr = strings.TrimSpace(nested.Expiry)
	}
	if access == "" && refresh == "" {
		return Credential{}, ErrNoCredentials
	}
	email := strings.TrimSpace(nested.IDTokenEmail)
	if email == "" {
		email = strings.TrimSpace(nested.Email)
	}
	cred := Credential{
		Email:        email,
		AccessToken:  access,
		RefreshToken: refresh,
	}
	if expiryStr != "" {
		if t, err := time.Parse(time.RFC3339Nano, expiryStr); err == nil {
			cred.Expiry = t
			cred.HasExpiry = true
		} else if t, err := time.Parse(time.RFC3339, expiryStr); err == nil {
			cred.Expiry = t
			cred.HasExpiry = true
		}
	}
	return cred, nil
}

// CredentialExpiresAt reports the OAuth access token's expiry timestamp in
// epoch milliseconds for provideraccounts.Policy.ChainPosition.
func CredentialExpiresAt(data []byte) (int64, bool) {
	cred, err := ParseCredential(data)
	if err != nil || !cred.HasExpiry || cred.Expiry.IsZero() {
		return 0, false
	}
	return cred.Expiry.UnixMilli(), true
}

// CredentialRefreshTokenFingerprint returns a hex SHA-256 fingerprint of the
// credential's refresh token for provideraccounts.Policy.TokenFingerprint.
func CredentialRefreshTokenFingerprint(data []byte) (string, bool) {
	cred, err := ParseCredential(data)
	if err != nil || cred.RefreshToken == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(cred.RefreshToken))
	return hex.EncodeToString(sum[:]), true
}

// IdentifyAccount queries Google's OAuth2 userinfo endpoint using the access
// token in credentialData and projects it into AO's provider.AccountInfo.
func IdentifyAccount(
	ctx context.Context,
	client *http.Client,
	credentialData []byte,
) (provider.AccountInfo, error) {
	return identifyAccountAtURL(ctx, client, googleUserInfoURL, credentialData)
}

func identifyAccountAtURL(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	credentialData []byte,
) (provider.AccountInfo, error) {
	cred, err := ParseCredential(credentialData)
	if err != nil {
		return provider.AccountInfo{}, err
	}
	if cred.AccessToken == "" {
		return provider.AccountInfo{}, ErrNoCredentials
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return provider.AccountInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return provider.AccountInfo{}, fmt.Errorf("agy: fetch Google userinfo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxUserInfoBytes+1))
	if readErr != nil {
		return provider.AccountInfo{}, fmt.Errorf("agy: read Google userinfo: %w", readErr)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return provider.AccountInfo{}, ErrOAuthUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return provider.AccountInfo{}, fmt.Errorf("agy: Google userinfo returned HTTP %d", resp.StatusCode)
	}

	var userInfo struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
		HD    string `json:"hd"`
	}
	if err := json.Unmarshal(body, &userInfo); err != nil {
		return provider.AccountInfo{}, fmt.Errorf("agy: decode Google userinfo: %w", err)
	}

	tokenSource := "oauth-personal"
	orgID := strings.TrimSpace(userInfo.ID)
	orgName := ""
	if hd := strings.TrimSpace(userInfo.HD); hd != "" {
		tokenSource = "oauth-business"
		orgName = hd
	}
	email := strings.TrimSpace(userInfo.Email)
	if email == "" {
		email = cred.Email
	}
	return provider.AccountInfo{
		Email:           email,
		DisplayName:     strings.TrimSpace(userInfo.Name),
		OrgID:           orgID,
		OrgName:         orgName,
		OrgNameReported: true,
		TokenSource:     tokenSource,
		APIProvider:     "google",
	}, nil
}

// ProbeConfig configures a zero-token Antigravity ACP account/tier probe.
// Opening and closing an ephemeral ACP session also lets agy refresh an expired
// access token in the target home.
type ProbeConfig struct {
	Binary  string
	WorkDir string
	// Home decides the canonical home the account manager reads, or answers
	// the isolated home of a sign-in or a saved login. It runs inside the
	// probe's time limit.
	Home   HomeSource
	Env    provider.EnvInput
	Reaper procutil.Reaper
}

// ProbeAccount dials an ephemeral `agy acp` process, opens and closes a session
// to verify authentication and read `_meta.antigravity.tier`, and returns the
// observed AccountInfo.
func ProbeAccount(ctx context.Context, cfg ProbeConfig) (provider.AccountInfo, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	binary := ResolveBinary(cfg.Binary)
	if strings.TrimSpace(binary) == "" {
		return provider.AccountInfo{}, errors.New("agy: binary path is empty")
	}
	workDir := cfg.WorkDir
	if strings.TrimSpace(workDir) == "" {
		workDir = "."
	}

	if cfg.Home == nil {
		return provider.AccountInfo{}, errors.New("agy: start account probe: launch without a home")
	}
	home, err := cfg.Home(probeCtx)
	if err != nil {
		return provider.AccountInfo{}, fmt.Errorf("agy: start account probe: %w", err)
	}
	launch, err := home.Launch(cfg.Env)
	if err != nil {
		return provider.AccountInfo{}, fmt.Errorf("agy: start account probe: %w", err)
	}

	conn, err := acp.Dial(probeCtx, acp.ConnConfig{
		Binary:       binary,
		Args:         launch.ACPArgs(),
		WorkDir:      workDir,
		Env:          launch.Env,
		Reaper:       cfg.Reaper,
		ThreadID:     "agy-account-probe",
		ProviderName: string(provider.Agy),
	})
	if err != nil {
		return provider.AccountInfo{}, fmt.Errorf("agy: start account probe: %w", err)
	}
	defer func() { _ = conn.Close() }()

	res, err := conn.NewRawSession(probeCtx, acp.SessionNewParams{
		CWD:        workDir,
		MCPServers: []acp.MCPServerConfig{},
	})
	if err != nil {
		return provider.AccountInfo{}, fmt.Errorf("agy: account probe session/new: %w", err)
	}
	if res.SessionID != "" {
		_ = conn.CloseRawSession(probeCtx, res.SessionID)
	}

	info := provider.AccountInfo{
		TokenSource: "oauth-personal",
		APIProvider: "google",
	}
	if len(res.Meta) > 0 {
		var meta struct {
			Antigravity *struct {
				Tier *struct {
					Name string `json:"name"`
					ID   string `json:"id"`
				} `json:"tier"`
			} `json:"antigravity"`
		}
		if json.Unmarshal(res.Meta, &meta) == nil && meta.Antigravity != nil && meta.Antigravity.Tier != nil {
			if name := strings.TrimSpace(meta.Antigravity.Tier.Name); name != "" {
				info.SubscriptionType = name
			}
		}
	}
	return info, nil
}
