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
		{"empty account", provider.AccountInfo{}, true},
		{"has subscription", provider.AccountInfo{SubscriptionType: "pro"}, false},
		{"has token source", provider.AccountInfo{TokenSource: "oauth"}, false},
		{"has both", provider.AccountInfo{SubscriptionType: "max", TokenSource: "oauth"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClaudeUnauthenticated(tc.info); got != tc.want {
				t.Fatalf("ClaudeUnauthenticated(%+v) = %v, want %v", tc.info, got, tc.want)
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
