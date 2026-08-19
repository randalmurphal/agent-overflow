package claudeconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, content string) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return New(path)
}

func TestReadOAuthAccountPresent(t *testing.T) {
	store := writeConfig(t, `{
		"numStartups": 4,
		"oauthAccount": {
			"accountUuid": "acct-1",
			"emailAddress": "User@Example.com",
			"organizationUuid": "org-uuid-1",
			"organizationName": "Example Org"
		}
	}`)
	record, ok := store.ReadOAuthAccount()
	if !ok {
		t.Fatal("expected record")
	}
	if record.OrganizationUUID != "org-uuid-1" || record.OrganizationName != "Example Org" {
		t.Fatalf("unexpected record: %+v", record)
	}
	if !record.EmailMatches("user@example.com") {
		t.Fatal("case-insensitive email should match")
	}
	if record.EmailMatches("other@example.com") {
		t.Fatal("different email must not match")
	}
	if record.EmailMatches("") {
		t.Fatal("blank probe email must not match: an unpaired record must not be trusted")
	}
}

func TestReadOAuthAccountAbsent(t *testing.T) {
	cases := map[string]string{
		"no key":     `{"numStartups": 4}`,
		"null":       `{"oauthAccount": null}`,
		"not object": `not json`,
	}
	for name, content := range cases {
		store := writeConfig(t, content)
		if _, ok := store.ReadOAuthAccount(); ok {
			t.Fatalf("%s: expected no record", name)
		}
	}
	missing := New(filepath.Join(t.TempDir(), ".claude.json"))
	if _, ok := missing.ReadOAuthAccount(); ok {
		t.Fatal("missing file: expected no record")
	}
}

func TestReadOAuthAccountBlankEmailRecordNeverMatches(t *testing.T) {
	store := writeConfig(t, `{"oauthAccount": {"organizationUuid": "org-1"}}`)
	record, ok := store.ReadOAuthAccount()
	if !ok {
		t.Fatal("expected record")
	}
	if record.EmailMatches("user@example.com") {
		t.Fatal("record without an email must not pair with any probe answer")
	}
}
