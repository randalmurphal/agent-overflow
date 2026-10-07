package repoidentity

import (
	"strings"
	"testing"
)

func TestRepositoryCoordinates(t *testing.T) {
	for _, tc := range []struct{ raw, safe, key string }{
		{"https://user:TOKEN@github.com/Owner/Repo.git?token=SECRET#frag", "https://github.com/Owner/Repo.git", "github.com/owner/repo"},
		{"ssh://git@ssh.github.com:443/Owner/Repo.git", "ssh://ssh.github.com:443/Owner/Repo.git", "github.com/owner/repo"},
		{"git@github.com:Owner/Repo.git", "git@github.com:Owner/Repo.git", "github.com/owner/repo"},
		{"user@self.example:Team/Repo.git", "self.example:Team/Repo.git", "self.example/Team/Repo"},
		{"file:///srv/repo", "file:///srv/repo", ""},
		{"https://user:%invalid@github.com/repo", "", ""},
	} {
		if got := SafeRemote(tc.raw); got != tc.safe {
			t.Errorf("safe %q = %q, want %q", tc.raw, got, tc.safe)
		}
		if got := Locator(tc.raw); got != tc.key {
			t.Errorf("key %q = %q, want %q", tc.raw, got, tc.key)
		}
	}
	if Matches("github:github.com:1", "github:github.com:2") {
		t.Fatal("conflicting IDs merged")
	}
	if !Matches("github:github.com:1", "github:github.com:1") {
		t.Fatal("renamed repository failed to match")
	}
	if got := RedactText("failed: https://user:SECRET@example.com/o/r?token=OTHER"); got != "failed: [remote]" {
		t.Fatal(got)
	}
}

func TestRedactTextKeepsQuotedPasswordsPrivate(t *testing.T) {
	for _, raw := range []string{
		"failed: 'https://user:sec'ret@github.com/a/b'",
		`failed: "https://user:sec'ret@github.com/a/b"`,
	} {
		if got := RedactText(raw); strings.Contains(got, "sec") || strings.Contains(got, "ret@") {
			t.Errorf("credential remains in %q", got)
		}
	}
}

func TestDiagnosticsContainNoRepositoryURL(t *testing.T) {
	for _, remote := range []string{"https://github.com/a/b", "git@github.com:a/b.git", "work-host:repo.git", "ssh://git@work-host/a/b"} {
		if got := RedactText("failed '" + remote + "'"); strings.Contains(got, remote) || !strings.Contains(got, "[remote]") {
			t.Fatalf("remote survived: %q", got)
		}
	}
}
