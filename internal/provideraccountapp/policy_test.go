package provideraccountapp

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/provider"
)

func TestCredentialPolicyIsProviderSpecific(t *testing.T) {
	husk := []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`)
	if !CredentialSignedOut("claude", husk) {
		t.Fatal("Claude sign-out marker was accepted as a credential")
	}
	if CredentialSignedOut("codex", husk) {
		t.Fatal("Claude credential shape was applied to Codex")
	}
	if got, ok := CredentialChainPosition("claude", []byte(`{"claudeAiOauth":{"expiresAt":1750000000000}}`)); !ok || got != 1750000000000 {
		t.Fatalf("Claude chain position = (%d, %v)", got, ok)
	}
	if _, ok := CredentialChainPosition("codex", husk); ok {
		t.Fatal("a Claude credential shape gave Codex a chain position")
	}
	codexAuth := []byte(`{"tokens":{"access_token":"a"},"last_refresh":"2026-10-15T02:48:19.5Z"}`)
	if got, ok := CredentialChainPosition("codex", codexAuth); !ok || got != 1792032499500 {
		t.Fatalf("Codex chain position = (%d, %v), want last_refresh in epoch ms", got, ok)
	}
}

func TestAccountProjectionTrimsObservedIdentity(t *testing.T) {
	account := AccountFromInfo("id", "claude", provider.AccountInfo{
		Email: " user@example.com ", OrgID: " org ", DisplayName: " User ",
	})
	if account.Email != "user@example.com" || account.OrgID != "org" || account.DisplayName != "User" {
		t.Fatalf("projected account = %+v", account)
	}
	if got := AccountInfo(account); got.Email != account.Email || got.OrgID != account.OrgID {
		t.Fatalf("round-trip info = %+v", got)
	}
}

func TestEnrichClaudeIdentityRequiresMatchingEmail(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	body := `{"oauthAccount":{"emailAddress":"user@example.com","organizationUuid":"org-1","organizationName":"Org"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config := claudeconfig.New(path)
	matched := EnrichClaudeIdentity(provider.AccountInfo{Email: "USER@example.com"}, config)
	if matched.OrgID != "org-1" || matched.OrgName != "Org" {
		t.Fatalf("matched identity = %+v", matched)
	}
	mismatched := EnrichClaudeIdentity(provider.AccountInfo{Email: "other@example.com"}, config)
	if mismatched.OrgID != "" {
		t.Fatalf("mismatched identity adopted org: %+v", mismatched)
	}
}

func TestValidateProviderRejectsUnknownProvider(t *testing.T) {
	if ValidateProvider("claude") != nil || ValidateProvider("codex") != nil {
		t.Fatal("managed provider rejected")
	}
	if ValidateProvider("other") == nil {
		t.Fatal("unknown provider accepted")
	}
}
