package claudeconfig

import (
	"encoding/json"
	"os"
	"testing"
)

const identityConfigBody = `{
  "numStartups": 42,
  "oauthAccount": {
    "accountUuid": "acct-1",
    "emailAddress": "one@example.com",
    "organizationUuid": "org-1"
  },
  "userID": "user-hash",
  "mcpServers": {
    "alpha": {"type": "stdio", "command": "/bin/alpha"}
  }
}`

func decodeConfig(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return out
}

func TestClearOAuthAccount_removesOnlyTheIdentity(t *testing.T) {
	store := newStoreWithFile(t, identityConfigBody)

	changed, err := store.ClearOAuthAccount()
	if err != nil {
		t.Fatalf("ClearOAuthAccount: %v", err)
	}
	if !changed {
		t.Fatal("expected the identity block to be reported as removed")
	}

	config := decodeConfig(t, store.path)
	if _, present := config["oauthAccount"]; present {
		t.Fatal("oauthAccount survived the clear")
	}
	// Everything else is Claude's business and must be untouched —
	// including userID, which Claude's own performLogout leaves alone.
	for _, key := range []string{"numStartups", "userID", "mcpServers"} {
		if _, present := config[key]; !present {
			t.Fatalf("clear dropped unrelated key %q", key)
		}
	}
	if string(config["userID"]) != `"user-hash"` {
		t.Fatalf("userID rewritten: %s", config["userID"])
	}
}

// A second clear must be a no-op that does not rewrite the file.
// Claude Code watches ~/.credentials.json and ~/.claude.json mtimes;
// an idempotent call that still bumps mtime would invalidate caches in
// every running CLI for no reason.
func TestClearOAuthAccount_absentIdentityDoesNotRewrite(t *testing.T) {
	store := newStoreWithFile(t, identityConfigBody)
	if _, err := store.ClearOAuthAccount(); err != nil {
		t.Fatalf("first clear: %v", err)
	}
	before, err := os.Stat(store.path)
	if err != nil {
		t.Fatalf("stat after first clear: %v", err)
	}

	changed, err := store.ClearOAuthAccount()
	if err != nil {
		t.Fatalf("second clear: %v", err)
	}
	if changed {
		t.Fatal("second clear reported a change")
	}

	after, err := os.Stat(store.path)
	if err != nil {
		t.Fatalf("stat after second clear: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("no-op clear rewrote the file")
	}
}

// A config that has never held a login is already in the desired
// post-condition, as is a missing file. Neither is an error, and
// neither should create or rewrite anything.
func TestClearOAuthAccount_missingFileAndEmptyConfig(t *testing.T) {
	missing := newStoreWithFile(t, "")
	changed, err := missing.ClearOAuthAccount()
	if err != nil {
		t.Fatalf("clear on missing file: %v", err)
	}
	if changed {
		t.Fatal("clear on missing file reported a change")
	}
	if _, err := os.Stat(missing.path); !os.IsNotExist(err) {
		t.Fatal("clear created a config file that did not exist")
	}

	empty := newStoreWithFile(t, `{"numStartups": 1}`)
	changed, err = empty.ClearOAuthAccount()
	if err != nil {
		t.Fatalf("clear on config without identity: %v", err)
	}
	if changed {
		t.Fatal("clear on config without identity reported a change")
	}
}

func TestStripOAuthAccount(t *testing.T) {
	stripped, err := StripOAuthAccount([]byte(identityConfigBody))
	if err != nil {
		t.Fatalf("StripOAuthAccount: %v", err)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(stripped, &config); err != nil {
		t.Fatalf("decode stripped config: %v", err)
	}
	if _, present := config["oauthAccount"]; present {
		t.Fatal("oauthAccount survived the strip")
	}
	if _, present := config["numStartups"]; !present {
		t.Fatal("strip dropped an unrelated key")
	}
}

// The strip runs over a file AO does not own. A shape we can't parse
// is passed through rather than rewritten or rejected — the copy is
// still useful, and only the identity claim is AO's concern.
func TestStripOAuthAccount_passesThroughUnknownShapes(t *testing.T) {
	for name, input := range map[string]string{
		"not json":      "definitely not json",
		"json array":    `["a", "b"]`,
		"no identity":   `{"numStartups": 1}`,
		"empty object":  `{}`,
		"empty content": ``,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := StripOAuthAccount([]byte(input))
			if err != nil {
				t.Fatalf("StripOAuthAccount(%q): %v", input, err)
			}
			if string(out) != input {
				t.Fatalf("input rewritten: got %q want %q", out, input)
			}
		})
	}
}
