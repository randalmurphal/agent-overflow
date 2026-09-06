package provideraccountapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

func TestClaudeTransferCredentialUsesNativeCredentialWhenProfileIsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		info       provider.AccountInfo
		credential string
		ready      bool
	}{
		{"missing", provider.AccountInfo{}, "", false},
		{"profile rebuilding", provider.AccountInfo{APIProvider: "firstParty", SubscriptionType: "Claude Max"}, `{"claudeAiOauth":{"accessToken":"fake","refreshToken":"fake","expiresAt":9999999999999}}`, true},
		{"destroyed login", provider.AccountInfo{APIProvider: "firstParty", SubscriptionType: "Claude Max"}, `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`, false},
		{"external backend", provider.AccountInfo{APIProvider: "bedrock"}, "", true},
		{"environment key", provider.AccountInfo{APIProvider: "firstParty", TokenSource: "apiKey"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var credential *provideraccounts.CredentialSnapshot
			if tc.credential != "" {
				credential = &provideraccounts.CredentialSnapshot{Data: []byte(tc.credential)}
			}
			if err := checkClaudeTransferCredential(tc.info, credential); (err == nil) != tc.ready {
				t.Fatal("wrong readiness", err)
			}
		})
	}
}

func TestTransferAdmissionCannotUseCachedAccountIdentity(t *testing.T) {
	m, _, _ := newTestManager(t)
	cache := provider.NewProbeCache(time.Minute)
	key := provider.ProbeCacheKey{Binary: "mock"}
	cache.Set(key, provider.AccountInfo{Email: "stale@example.test"})
	calls := 0
	refusal := errors.New("fresh credential missing")
	_, err := m.RunAccountProbe(ProbeRequest{ProviderName: "claude", Cache: cache, Key: key,
		Probe: func(context.Context) (provider.AccountInfo, error) { calls++; return provider.AccountInfo{}, nil },
		Validate: func(info provider.AccountInfo, credential *provideraccounts.CredentialSnapshot) error {
			if info.Email != "" || credential != nil {
				t.Fatal("admission used stale identity")
			}
			return refusal
		},
	})
	if !errors.Is(err, refusal) || calls != 1 {
		t.Fatalf("fresh admission: calls=%d %v", calls, err)
	}
}
