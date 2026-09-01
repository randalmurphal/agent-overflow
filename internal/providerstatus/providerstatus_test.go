package providerstatus

import (
	"testing"

	"agent-overflow/internal/provider"
)

func TestEventFromDetectMapsVersionTooOld(t *testing.T) {
	in := provider.ProviderStatus{
		Provider:   string(provider.Codex),
		Installed:  true,
		Version:    "codex 0.36.0",
		BinaryPath: "/usr/local/bin/codex",
		Status:     "version_too_old",
		Message:    "Codex CLI v0.36.0 is too old for Agent Overflow. Upgrade to v0.37.0 or newer and restart the app.",
	}
	got := EventFromDetect(in)
	if got.Status != "version_too_old" {
		t.Fatalf("Status = %q, want version_too_old", got.Status)
	}
	if got.Message != in.Message {
		t.Fatalf("Message = %q, want %q (passthrough)", got.Message, in.Message)
	}
	if got.Version != in.Version {
		t.Fatalf("Version = %q, want %q", got.Version, in.Version)
	}
	if !got.Actionable {
		t.Fatal("version_too_old must be actionable")
	}
	// version_too_old does NOT carry an ActionURL — the remediation is
	// "run your package manager + restart the app", which isn't a link.
	if got.ActionURL != "" {
		t.Fatalf("ActionURL = %q, want empty for version_too_old", got.ActionURL)
	}
}

func TestEventFromDetectSkipsActionForReady(t *testing.T) {
	in := provider.ProviderStatus{
		Provider: string(provider.Claude),
		Status:   "ready",
		Version:  "claude-code 2.0.0",
	}
	got := EventFromDetect(in)
	if got.Status != "ready" {
		t.Fatalf("Status = %q, want ready", got.Status)
	}
	if got.Actionable {
		t.Fatal("ready must not be actionable — banner should be empty")
	}
	if got.Message != "" {
		t.Fatalf("Message = %q, want empty for ready", got.Message)
	}
	if got.ActionURL != "" {
		t.Fatalf("ActionURL = %q, want empty for ready", got.ActionURL)
	}
}

func TestEventFromDetectPopulatesActionURLForNotFound(t *testing.T) {
	in := provider.ProviderStatus{
		Provider: string(provider.Claude),
		Status:   "not_found",
		Message:  "claude binary not found on PATH",
	}
	got := EventFromDetect(in)
	if got.ActionURL == "" {
		t.Fatal("not_found must carry an ActionURL (Go owns the URL table)")
	}
	if !got.Actionable {
		t.Fatal("not_found must be actionable")
	}
}

func TestClaudeUnauthenticated(t *testing.T) {
	cases := []struct {
		name string
		info provider.AccountInfo
		want bool
	}{
		// The only logged-out signal: the CLI reported no account identity
		// whatsoever.
		{"empty account", provider.AccountInfo{}, true},

		// firstParty is what the CLI reports for the direct Anthropic
		// backend regardless of login state, so on its own it is not
		// evidence of an account.
		{"bare firstParty", provider.AccountInfo{APIProvider: "firstParty"}, true},

		// Every non-firstParty backend short-circuits the CLI's account
		// builder: apiProvider is the ONLY field a fully working account
		// surfaces (verified against claude 2.1.219). Each of these was
		// false-flagged as logged out by the subscription-or-token rule.
		{"bedrock", provider.AccountInfo{APIProvider: "bedrock"}, false},
		{"vertex", provider.AccountInfo{APIProvider: "vertex"}, false},
		{"gateway", provider.AccountInfo{APIProvider: "gateway"}, false},
		{"foundry", provider.AccountInfo{APIProvider: "foundry"}, false},
		{"anthropicAws", provider.AccountInfo{APIProvider: "anthropicAws"}, false},
		{"anthropicGoogleCloud", provider.AccountInfo{APIProvider: "anthropicGoogleCloud"}, false},
		{"mantle", provider.AccountInfo{APIProvider: "mantle"}, false},

		// firstParty with a profile-sourced token: tokenSource is not
		// populated, email is. Also false-flagged before.
		{"firstParty profile", provider.AccountInfo{Email: "user@example.com", APIProvider: "firstParty"}, false},

		// firstParty, fully populated — the case the old rule got right.
		{"firstParty populated", provider.AccountInfo{
			Email:            "user@example.com",
			SubscriptionType: "Claude Max",
			TokenSource:      "claude.ai",
			APIProvider:      "firstParty",
		}, false},

		// Individual evidence fields, each sufficient on its own.
		{"token source only", provider.AccountInfo{TokenSource: "oauth"}, false},
		{"email only", provider.AccountInfo{Email: "user@example.com"}, false},
		{"display name only", provider.AccountInfo{DisplayName: "Ada"}, false},

		// subscriptionType is a storage echo, not identity. Spike-verified on
		// 2.1.232 (2026-08-16): the husk left by a failed refresh retains the
		// field, and the probe echoes the plan label of a login that no longer
		// exists. Counting it let app code commit the husk over the account's
		// credential slot and destroy it.
		{"husk echo", provider.AccountInfo{
			SubscriptionType: "Claude Max",
			APIProvider:      "firstParty",
		}, true},
		{"subscription only", provider.AccountInfo{SubscriptionType: "pro"}, true},
		// A real subscription login reports its email alongside the plan.
		{"subscription with email", provider.AccountInfo{
			SubscriptionType: "Claude Max",
			Email:            "user@example.com",
			APIProvider:      "firstParty",
		}, false},

		// `tokenSource:"none"` is the CLI's explicit no-token marker, not a
		// token source. Spike-verified on 2.1.219 (2026-08-03): after a failed
		// startup refresh blanks the stored tokens, initialize reports exactly
		// {tokenSource:"none", apiProvider:"firstParty"} — a destroyed login
		// that must read as logged out.
		{"blanked login", provider.AccountInfo{
			TokenSource: "none",
			APIProvider: "firstParty",
		}, true},
		// The union of what both spikes saw on a destroyed login: 2.1.219
		// reported the explicit no-token marker, 2.1.232 the retained plan
		// label. A build that reports both at once must still read as logged
		// out — neither field is identity, and the pair is what the CLI has
		// left after blanking the tokens.
		{"blanked login with a retained plan label", provider.AccountInfo{
			SubscriptionType: "Claude Max",
			TokenSource:      "none",
			APIProvider:      "firstParty",
		}, true},
		{"token source none only", provider.AccountInfo{TokenSource: "none"}, true},
		{"token source none padded", provider.AccountInfo{TokenSource: " None "}, true},
		// "none" must not veto real evidence riding alongside it.
		{"token source none with email", provider.AccountInfo{
			TokenSource: "none",
			Email:       "user@example.com",
		}, false},

		// Whitespace is not identity. A padded field must not read as
		// evidence, and must not rescue an otherwise-empty account.
		{"whitespace fields", provider.AccountInfo{
			Email:            "  ",
			SubscriptionType: "\t",
			TokenSource:      " \n",
			DisplayName:      " ",
		}, true},
		{"whitespace apiProvider", provider.AccountInfo{APIProvider: "   "}, true},
		{"padded bedrock", provider.AccountInfo{APIProvider: " bedrock "}, false},
		{"padded firstParty", provider.AccountInfo{APIProvider: " firstParty "}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClaudeUnauthenticated(tc.info); got != tc.want {
				t.Fatalf("ClaudeUnauthenticated(%+v) = %v, want %v", tc.info, got, tc.want)
			}
		})
	}
}

// TestClaudeUnauthenticatedCoversEveryAPIProvider pins the predicate to the
// full apiProvider enum the CLI can report, so a member added upstream that
// nobody thought to add to the table above still gets an answer asserted
// here. Only firstParty may be treated as "no evidence" — every other member
// is reachable ONLY when a real external-credential backend is configured.
func TestClaudeUnauthenticatedCoversEveryAPIProvider(t *testing.T) {
	// The enum as of claude 2.1.219.
	all := []string{
		"gateway", "bedrock", "foundry", "anthropicAws",
		"anthropicGoogleCloud", "mantle", "vertex", "firstParty",
	}
	for _, apiProvider := range all {
		t.Run(apiProvider, func(t *testing.T) {
			bare := ClaudeUnauthenticated(provider.AccountInfo{APIProvider: apiProvider})
			wantBare := apiProvider == "firstParty"
			if bare != wantBare {
				t.Fatalf("bare apiProvider=%q: unauthenticated = %v, want %v", apiProvider, bare, wantBare)
			}
			// Whatever the backend, a populated account is authenticated.
			populated := provider.AccountInfo{
				Email:            "user@example.com",
				SubscriptionType: "Claude Max",
				TokenSource:      "claude.ai",
				APIProvider:      apiProvider,
			}
			if ClaudeUnauthenticated(populated) {
				t.Fatalf("populated apiProvider=%q reported unauthenticated", apiProvider)
			}
		})
	}
}

func TestActionURLTable(t *testing.T) {
	cases := []struct {
		provider string
		status   string
		wantSet  bool
	}{
		{string(provider.Claude), "not_found", true},
		{string(provider.Claude), "unauthenticated", true},
		{string(provider.Codex), "not_found", true},
		{string(provider.Codex), "version_too_old", false}, // "upgrade + restart", no single URL
		{string(provider.Claude), "ready", false},
		{string(provider.Claude), "error", false},
	}
	for _, tc := range cases {
		got := ActionURL(tc.provider, tc.status)
		if tc.wantSet && got == "" {
			t.Fatalf("ActionURL(%q, %q) = empty, want a URL", tc.provider, tc.status)
		}
		if !tc.wantSet && got != "" {
			t.Fatalf("ActionURL(%q, %q) = %q, want empty", tc.provider, tc.status, got)
		}
	}
}

func TestVersionTokenReadsEveryVersionSurface(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		// `claude --version`
		{"2.1.257 (Claude Code)", "2.1.257"},
		// `codex --version`
		{"codex-cli 0.149.0", "0.149.0"},
		// Claude `system/init`'s claude_code_version, Codex's parsed
		// app-server version: already bare.
		{"2.1.257", "2.1.257"},
		{"0.149.0", "0.149.0"},
		{"v1.2", "1.2"},
		{"1.2.", "1.2"},
		{"", ""},
		{"unknown", ""},
	}
	for _, tc := range cases {
		if got := VersionToken(tc.raw); got != tc.want {
			t.Errorf("VersionToken(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestBinaryStaleTreatsUnknownAsNotStale(t *testing.T) {
	cases := []struct {
		name      string
		session   string
		installed string
		want      bool
	}{
		{"upgraded underneath the session", "2.1.257", "2.1.258 (Claude Code)", true},
		{"same build, different surfaces", "0.149.0", "codex-cli 0.149.0", false},
		// One side normalizes two-segment versions to three; a textual
		// compare would report an upgrade that never happened.
		{"trailing zero segment", "0.149.0", "codex-cli 0.149", false},
		{"session never reported a version", "", "2.1.258", false},
		{"binary version unreadable", "2.1.257", "", false},
		{"neither side known", "", "", false},
		{"nothing numeric on either side", "unknown", "garbled", false},
	}
	for _, tc := range cases {
		if got := BinaryStale(tc.session, tc.installed); got != tc.want {
			t.Errorf("%s: BinaryStale(%q, %q) = %v, want %v", tc.name, tc.session, tc.installed, got, tc.want)
		}
	}
}
