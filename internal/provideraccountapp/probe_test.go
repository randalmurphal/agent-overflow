package provideraccountapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

type recordingAccountSink struct {
	events  []string
	signIns []SignIn
	// changed is the provider of each PublishAccountsChanged.
	changed []string
	// listings is the provider of each PublishListingChanged.
	listings []string
	// limitsReset is the account of each PublishLimitsReset.
	limitsReset []string
	// onListing, when set, runs at each PublishListingChanged.
	onListing func()
}

func (s *recordingAccountSink) PublishAccount(string, provideraccounts.Account, provider.AccountInfo, uint64) {
	s.events = append(s.events, "account")
}
func (s *recordingAccountSink) PublishCleared(string, uint64) { s.events = append(s.events, "cleared") }
func (s *recordingAccountSink) PublishAccountsChanged(providerName string) {
	s.events = append(s.events, "changed")
	s.changed = append(s.changed, providerName)
}
func (s *recordingAccountSink) PublishListingChanged(providerName string) {
	s.events = append(s.events, "listing")
	s.listings = append(s.listings, providerName)
	if s.onListing != nil {
		s.onListing()
	}
}
func (s *recordingAccountSink) PublishLimitsReset(_, accountID string) {
	s.events = append(s.events, "limits-reset")
	s.limitsReset = append(s.limitsReset, accountID)
}
func (s *recordingAccountSink) PublishUsageError(string, string, error) {
	s.events = append(s.events, "usage-error")
}
func (s *recordingAccountSink) PublishLogin(LoginState) { s.events = append(s.events, "login") }
func (s *recordingAccountSink) PublishSignIn(signIn SignIn) {
	s.events = append(s.events, "signin")
	s.signIns = append(s.signIns, signIn)
}

// A client refreshes the model catalog on `provider:account`. AfterAdopt is
// where Claude commits that catalog, so it must land before the emit.
func TestRunAccountProbeRunsAfterAdoptBeforeTheAccountEmit(t *testing.T) {
	sink := &recordingAccountSink{}
	manager := NewManager(Deps{Reaper: procutiltest.Reaper(), Accounts: sink})

	_, err := manager.RunAccountProbe(ProbeRequest{
		ProviderName: string(provider.Claude),
		Probe: func(context.Context) (provider.AccountInfo, error) {
			return provider.AccountInfo{SubscriptionType: "max"}, nil
		},
		AfterAdopt: func(provideraccounts.Account) { sink.events = append(sink.events, "after-adopt") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 || sink.events[0] != "after-adopt" || sink.events[1] != "account" {
		t.Fatalf("events = %v, want [after-adopt account]", sink.events)
	}
}

// Every canonical agy probe the manager starts (account probes, reconciliation,
// the token refresh, the transfer check) runs on the store and home the
// manager reads, whatever the process inherited: here the file store, so the
// flag is set, and the home variables are cleared. The home is agy's default
// when the process HOME holds the manager's directory, and that directory
// named when it does not. A sign-in's isolated home is on file storage in its
// own directory.
func TestAgyProbesRunOnTheManagersStoreAndHome(t *testing.T) {
	manager, _, credentials := newTestManager(t)
	// After the fixture, which detaches the provider homes: these stand for
	// what the environment Agent Overflow inherited says.
	t.Setenv("AGY_ACP_FORCE_FILE_STORAGE", "")
	t.Setenv("GEMINI_HOME", filepath.Join(t.TempDir(), "other-gemini"))
	t.Setenv("AGY_APP_DATA_DIR", filepath.Join(t.TempDir(), "other-app-data"))
	binary, record := agyAccountProbeRecording(t)
	agyName := string(provider.Agy)

	paths, err := credentials.Paths(agyName)
	if err != nil {
		t.Fatal(err)
	}
	managerDir := paths.SharedHome
	_, probe := manager.AgyAccountProbe(binary, "")

	t.Setenv("HOME", t.TempDir())
	if _, err := probe(context.Background()); err != nil {
		t.Fatalf("canonical probe: %v", err)
	}
	got := readAgyLaunchRecord(t, record)
	named := "acp --gemini-dir " + filepath.Dir(managerDir) + " --app-data-dir " + filepath.Base(managerDir)
	if got["AGY_ACP_FORCE_FILE_STORAGE"] != "1" || got["GEMINI_HOME"] != "<unset>" || got["AGY_APP_DATA_DIR"] != "<unset>" || got["args"] != named {
		t.Fatalf("canonical probe under another HOME launched with %v, want file storage in %s", got, managerDir)
	}

	t.Setenv("HOME", filepath.Dir(filepath.Dir(managerDir)))
	if _, err := probe(context.Background()); err != nil {
		t.Fatalf("canonical probe: %v", err)
	}
	got = readAgyLaunchRecord(t, record)
	if got["AGY_ACP_FORCE_FILE_STORAGE"] != "1" || got["GEMINI_HOME"] != "<unset>" || got["AGY_APP_DATA_DIR"] != "<unset>" || got["args"] != "acp" {
		t.Fatalf("canonical probe launched with %v, want file storage on agy's default home", got)
	}

	ephemeral, err := credentials.NewEphemeralHome(agyName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ephemeral.Cleanup() })
	if _, err := manager.probeProviderAccountAtHome(agyName, binary, ephemeral.Path); err != nil {
		t.Fatalf("isolated probe: %v", err)
	}
	got = readAgyLaunchRecord(t, record)
	want := "acp --gemini-dir " + filepath.Dir(ephemeral.Path) + " --app-data-dir " + filepath.Base(ephemeral.Path)
	if got["AGY_ACP_FORCE_FILE_STORAGE"] != "1" || got["GEMINI_HOME"] != "<unset>" || got["AGY_APP_DATA_DIR"] != "<unset>" || got["args"] != want {
		t.Fatalf("isolated probe launched with %v, want file storage in %s", got, ephemeral.Path)
	}
}

// Without its credential store the manager reads no login, so it names no
// store for one either.
func TestAgyCanonicalHomeNeedsTheCredentialStore(t *testing.T) {
	manager := NewManager(Deps{Reaper: procutiltest.Reaper()})
	if _, err := manager.AgyCanonicalHome(context.Background()); err == nil {
		t.Fatal("a manager without credentials chose an Antigravity store")
	}
}

// A relative process HOME is not agy's default home: the canonical home is
// then the manager's directory, which under that HOME is relative too, and
// is refused rather than launched.
func TestAgyCanonicalHomeRefusesARelativeHome(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", "relative-home")
	store, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provideraccounts.NewCredentialsWithFileKeychain("relative-home", CredentialPolicy())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(Deps{Reaper: procutiltest.Reaper()})
	if err := manager.Attach(store, credentials, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AgyCanonicalHome(context.Background()); err == nil || !strings.Contains(err.Error(), "is not an absolute path") {
		t.Fatalf("AgyCanonicalHome under a relative HOME = %v, want the relative directory refused", err)
	}
}

func readAgyLaunchRecord(t *testing.T, record string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read the mock agy's launch record: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, value, _ := strings.Cut(line, "=")
		out[name] = value
	}
	return out
}

func TestIdentifyAgyCredentialOfflineEmailFallback(t *testing.T) {
	manager := NewManager(Deps{Reaper: procutiltest.Reaper()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // force HTTP IdentifyAccount to fail offline immediately
	cred := []byte(`{"access_token":"tok","refresh_token":"ref","id_token_email":"offline@example.com"}`)
	got, err := manager.identifyAgyCredential(ctx, provider.AccountInfo{SubscriptionType: "Google AI Ultra"}, cred)
	if err != nil {
		t.Fatalf("a failed lookup of a credential that carries its email = %v, want the email", err)
	}
	if got.Email != "offline@example.com" {
		t.Fatalf("Email = %q, want offline@example.com", got.Email)
	}
	if got.SubscriptionType != "Google AI Ultra" {
		t.Fatalf("SubscriptionType = %q, want Google AI Ultra", got.SubscriptionType)
	}
}
