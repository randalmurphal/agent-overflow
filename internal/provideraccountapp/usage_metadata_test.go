package provideraccountapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/testutil/mockexec"
)

// accountPublishSink records the cards PublishAccount reported.
type accountPublishSink struct {
	recordingAccountSink
	published []provideraccounts.Account
}

func (s *accountPublishSink) PublishAccount(
	providerName string,
	account provideraccounts.Account,
	info provider.AccountInfo,
	generation uint64,
) {
	s.recordingAccountSink.PublishAccount(providerName, account, info, generation)
	s.published = append(s.published, account)
}

// codexUsageProbe is a mock `codex app-server` for the usage probe whose
// account/read answer is the JSON-RPC frame in the returned file, so a test
// can change what Codex reports between refreshes.
func codexUsageProbe(t *testing.T) (binary, accountFrame string) {
	t.Helper()
	dir := t.TempDir()
	accountFrame = filepath.Join(dir, "account-read.json")
	rateLimits := `{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"limitId":"codex","planType":"team",` +
		`"primary":{"usedPercent":10,"windowDurationMins":300,"resetsAt":4102444800}}}}`
	binary = mockexec.Write(t, filepath.Join(dir, "mock-codex"), "#!/bin/bash\n"+
		"read -r _ || true\nread -r _ || true\nread -r _ || true\nread -r _ || true\n"+
		"cat "+shellQuoteForTest(accountFrame)+"\n"+
		"printf '%s\\n' '"+rateLimits+"'\n")
	return binary, accountFrame
}

func shellQuoteForTest(s string) string { return "'" + s + "'" }

func writeAccountReadFrame(t *testing.T, path, frame string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(frame+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func chatgptAccountFrame(email, planType, workspaceField string) string {
	return `{"jsonrpc":"2.0","id":3,"result":{"account":{"type":"chatgpt","email":"` + email +
		`","planType":"` + planType + `"` + workspaceField + `},"requiresOpenaiAuth":true}}`
}

func newCodexUsageMetadataManager(t *testing.T, saved provideraccounts.Account) (
	*Manager,
	*provideraccounts.Store,
	*accountPublishSink,
	string,
) {
	t.Helper()
	manager, store, credentials := newTestManager(t)
	sink := &accountPublishSink{}
	manager.deps.Accounts = sink
	binary, accountFrame := codexUsageProbe(t)
	manager.deps.ProviderBinary = func(string) string { return binary }
	if _, err := store.Add(saved); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential(
		saved.Provider,
		saved.ID,
		[]byte(`{"tokens":{"access_token":"inactive","refresh_token":"inactive-refresh"}}`),
	); err != nil {
		t.Fatal(err)
	}
	return manager, store, sink, accountFrame
}

// A usage refresh of a saved, inactive Codex account is the one place that
// account's account/read is seen until it is selected again, so the workspace
// name and plan it reports land on the saved card there. The listing is
// republished only when the stored row changed, and the card never travels on
// the selected account's event, which every client shows as the one in use.
func TestCodexUsageRefreshRecordsObservedWorkspaceName(t *testing.T) {
	codexName := string(provider.Codex)
	manager, store, sink, accountFrame := newCodexUsageMetadataManager(t, provideraccounts.Account{
		ID: "inactive", Provider: codexName, Email: "inactive@example.com",
		SubscriptionType: "team", APIProvider: "openai",
	})

	steps := []struct {
		name          string
		frame         string
		wantOrgName   string
		wantPlan      string
		wantPublished bool
	}{
		{"blank", chatgptAccountFrame("inactive@example.com", "team", ""), "", "team", false},
		{"named", chatgptAccountFrame("inactive@example.com", "team", `,"workspaceName":"Acme"`), "Acme", "team", true},
		{"same name again", chatgptAccountFrame("inactive@example.com", "team", `,"workspaceName":"Acme"`), "Acme", "team", false},
		{"renamed", chatgptAccountFrame("inactive@example.com", "team", `,"workspaceName":"Acme Renamed"`), "Acme Renamed", "team", true},
		{"absent", chatgptAccountFrame("inactive@example.com", "team", ""), "", "team", true},
		{"plan changed", chatgptAccountFrame("inactive@example.com", "enterprise", `,"workspaceName":null`), "", "enterprise", true},
	}
	for _, step := range steps {
		writeAccountReadFrame(t, accountFrame, step.frame)
		before := len(sink.listings)
		if err := manager.RefreshProviderAccountUsage(codexName, "inactive"); err != nil {
			t.Fatalf("%s: RefreshProviderAccountUsage: %v", step.name, err)
		}
		saved, ok := store.Get(codexName, "inactive", time.Now())
		if !ok {
			t.Fatalf("%s: account missing", step.name)
		}
		if saved.OrgName != step.wantOrgName || saved.SubscriptionType != step.wantPlan ||
			saved.Email != "inactive@example.com" || saved.APIProvider != "openai" {
			t.Fatalf("%s: saved = %+v, want OrgName %q plan %q", step.name, saved, step.wantOrgName, step.wantPlan)
		}
		if _, active := store.Active(codexName, time.Now()); active {
			t.Fatalf("%s: usage refresh selected the account", step.name)
		}
		wantListings := 0
		if step.wantPublished {
			wantListings = 1
		}
		if listings := len(sink.listings) - before; listings != wantListings {
			t.Fatalf("%s: listing changes = %d, want %d", step.name, listings, wantListings)
		}
		if len(sink.published) != 0 {
			t.Fatalf("%s: an inactive card went out as the selected account: %+v", step.name, sink.published)
		}
	}
}

// Only a successful chatgpt account/read that names the login says whether a
// workspace name exists. Any other answer leaves the saved name alone.
func TestCodexUsageRefreshKeepsWorkspaceNameWithoutAnAccountRead(t *testing.T) {
	codexName := string(provider.Codex)
	cases := []struct {
		name  string
		frame string
	}{
		{"failed account/read", `{"jsonrpc":"2.0","id":3,"error":{"code":-32603,"message":"boom"}}`},
		{"api key account", `{"jsonrpc":"2.0","id":3,"result":{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}}`},
		{"null email", `{"jsonrpc":"2.0","id":3,"result":{"account":{"type":"chatgpt","email":null,"planType":"team"},"requiresOpenaiAuth":true}}`},
		{"no account", `{"jsonrpc":"2.0","id":3,"result":{"account":null,"requiresOpenaiAuth":true}}`},
		{"undecodable account/read", `{"jsonrpc":"2.0","id":3,"result":{"account":"not an object"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager, store, sink, accountFrame := newCodexUsageMetadataManager(t, provideraccounts.Account{
				ID: "inactive", Provider: codexName, Email: "inactive@example.com",
				OrgName: "Acme", SubscriptionType: "team", APIProvider: "openai",
			})
			writeAccountReadFrame(t, accountFrame, tc.frame)
			if err := manager.RefreshProviderAccountUsage(codexName, "inactive"); err != nil {
				t.Fatalf("RefreshProviderAccountUsage: %v", err)
			}
			saved, _ := store.Get(codexName, "inactive", time.Now())
			if saved.OrgName != "Acme" || saved.Email != "inactive@example.com" {
				t.Fatalf("saved = %+v, want the Acme name kept", saved)
			}
			if len(sink.published) != 0 {
				t.Fatalf("published %+v without an identity observation", sink.published)
			}
		})
	}
}

// A refresh whose login turns out to be another saved account fails, but what
// it observed is that account's and is recorded there, and the listing says
// so.
func TestCodexUsageRefreshRecordsMetadataOnTheAccountItObserved(t *testing.T) {
	codexName := string(provider.Codex)
	manager, store, sink, accountFrame := newCodexUsageMetadataManager(t, provideraccounts.Account{
		ID: "refreshed", Provider: codexName, Email: "refreshed@example.com", SubscriptionType: "team",
	})
	if _, err := store.Add(provideraccounts.Account{
		ID: "other", Provider: codexName, Email: "other@example.com", SubscriptionType: "team",
	}); err != nil {
		t.Fatal(err)
	}
	writeAccountReadFrame(t, accountFrame,
		chatgptAccountFrame("other@example.com", "team", `,"workspaceName":"Other Co"`))

	err := manager.RefreshProviderAccountUsage(codexName, "refreshed")
	if err == nil || !strings.Contains(err.Error(), "is other@example.com (Other Co)'s") {
		t.Fatalf("RefreshProviderAccountUsage = %v, want the attribution failure", err)
	}
	if refreshed, _ := store.Get(codexName, "refreshed", time.Now()); refreshed.OrgName != "" {
		t.Fatalf("refreshed account took another login's name: %+v", refreshed)
	}
	other, _ := store.Get(codexName, "other", time.Now())
	if other.OrgName != "Other Co" {
		t.Fatalf("observed account = %+v, want OrgName Other Co", other)
	}
	if len(sink.listings) != 1 || len(sink.published) != 0 {
		t.Fatalf("listing changes = %v, cards published = %+v; want one listing change and no selected-account card",
			sink.listings, sink.published)
	}
}
