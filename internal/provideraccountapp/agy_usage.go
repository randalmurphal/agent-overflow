package provideraccountapp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/agy"
)

// probeSelectedAgyRateLimits fetches Antigravity's live quota summary over HTTP
// for the currently selected account. If the access token is rejected with 401,
// it runs one zero-token ACP probe in the canonical home so agy itself refreshes
// the OAuth access token, then re-reads the updated credential and retries once.
func (m *Manager) probeSelectedAgyRateLimits(
	ctx context.Context,
	credential []byte,
) (provider.RateLimitsSnapshot, []byte, error) {
	snapshot, err := agy.ProbeRateLimitsFromCredentialData(ctx, m.providerHTTPClient(), credential)
	if !errors.Is(err, agy.ErrOAuthUnauthorized) {
		return snapshot, nil, err
	}
	// Let agy refresh its own access token in the canonical home (it holds no
	// OAuth client secret in the token file).
	binary := m.providerBinaryPath(string(provider.Agy))
	if _, refreshErr := agy.ProbeAccount(ctx, m.agyProbeConfig(binary, "", "", nil)); refreshErr != nil {
		return provider.RateLimitsSnapshot{}, nil, fmt.Errorf("refresh Antigravity OAuth token: %w", refreshErr)
	}
	refreshed, present, readErr := m.readCanonicalCredentialIfPresent(string(provider.Agy))
	if readErr != nil || !present {
		return provider.RateLimitsSnapshot{}, nil, errors.New("Antigravity credentials missing after refresh; sign in again")
	}
	rotated := refreshed.Bytes()
	snapshot, err = agy.ProbeRateLimitsFromCredentialData(ctx, m.providerHTTPClient(), rotated)
	return snapshot, rotated, err
}

// probeInactiveAgyRateLimits reads an inactive Antigravity account's quota over
// HTTP without touching the canonical home or refreshing tokens.
func (m *Manager) probeInactiveAgyRateLimits(
	ctx context.Context,
	credential []byte,
) (provider.RateLimitsSnapshot, error) {
	return agy.ProbeRateLimitsFromCredentialData(ctx, m.providerHTTPClient(), credential)
}

// identifyAgyCredential enriches a probed Antigravity AccountInfo with email,
// display name, and Google account/workspace identity from userinfo when
// available. Non-fatal on offline/test networks if tier info is already present.
func (m *Manager) identifyAgyCredential(
	ctx context.Context,
	info provider.AccountInfo,
	credential []byte,
) provider.AccountInfo {
	if len(credential) == 0 {
		return info
	}
	identified, err := agy.IdentifyAccount(ctx, m.providerHTTPClient(), credential)
	if err != nil {
		return info
	}
	if identified.Email != "" {
		info.Email = identified.Email
	}
	if identified.DisplayName != "" {
		info.DisplayName = identified.DisplayName
	}
	if identified.OrgID != "" {
		info.OrgID = identified.OrgID
	}
	if identified.OrgNameReported {
		info.OrgName = identified.OrgName
		info.OrgNameReported = true
	}
	if identified.TokenSource != "" {
		info.TokenSource = identified.TokenSource
	}
	if identified.APIProvider != "" {
		info.APIProvider = identified.APIProvider
	}
	return info
}

func splitAgyEphemeralHome(home string) (geminiDir, appDataSubdir string) {
	cleaned := filepath.Clean(strings.TrimSpace(home))
	if cleaned == "" || cleaned == "." {
		return "", ""
	}
	return filepath.Dir(cleaned), filepath.Base(cleaned)
}
