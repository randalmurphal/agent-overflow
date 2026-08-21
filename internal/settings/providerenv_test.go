package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetProviderEnvVarRoundTripsNameValueAndSensitiveFlag(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if _, err := svc.SetProviderEnvVar("claude", "ANTHROPIC_BASE_URL", "https://gw.example.test", false); err != nil {
		t.Fatalf("SetProviderEnvVar() error = %v", err)
	}
	got, err := svc.SetProviderEnvVar("claude", "PROXY_TOKEN", "s3cret", true)
	if err != nil {
		t.Fatalf("SetProviderEnvVar() error = %v", err)
	}
	want := []ProviderEnvVar{
		{Name: "ANTHROPIC_BASE_URL", Value: "https://gw.example.test"},
		{Name: "PROXY_TOKEN", Value: "s3cret", Sensitive: true},
	}
	assertEnvVars(t, got.ClaudeCustomEnv, want)

	// A fresh service reads the same list back off disk, flags included.
	assertEnvVars(t, NewService(dir).Get().ClaudeCustomEnv, want)

	// The two providers are separate lists.
	if len(got.CodexCustomEnv) != 0 {
		t.Fatalf("CodexCustomEnv = %v, want empty", got.CodexCustomEnv)
	}
}

func TestSetProviderEnvVarReplacesInPlaceAndDeleteRemoves(t *testing.T) {
	svc := NewService(t.TempDir())

	mustSetEnvVar(t, svc, "codex", "FIRST", "1", false)
	mustSetEnvVar(t, svc, "codex", "SECOND", "2", false)
	// Replacing keeps the row where the user put it, and can flip the flag.
	got := mustSetEnvVar(t, svc, "codex", "first", "1-updated", true)
	assertEnvVars(t, got.CodexCustomEnv, []ProviderEnvVar{
		{Name: "first", Value: "1-updated", Sensitive: true},
		{Name: "SECOND", Value: "2"},
	})

	got, err := svc.DeleteProviderEnvVar("codex", "FIRST")
	if err != nil {
		t.Fatalf("DeleteProviderEnvVar() error = %v", err)
	}
	assertEnvVars(t, got.CodexCustomEnv, []ProviderEnvVar{{Name: "SECOND", Value: "2"}})

	if _, err := svc.DeleteProviderEnvVar("codex", "FIRST"); err == nil {
		t.Fatal("deleting an absent variable should error, not silently no-op")
	}
}

// The last delete must clear the key entirely so sparse serialization omits it
// — an empty list left behind would defeat the "differs from default" check
// and start shipping `"codexCustomEnv": []` in every write.
func TestDeleteProviderEnvVarLeavesNoResidue(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	mustSetEnvVar(t, svc, "codex", "ONLY", "1", false)
	if _, err := svc.DeleteProviderEnvVar("codex", "ONLY"); err != nil {
		t.Fatalf("DeleteProviderEnvVar() error = %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]json.RawMessage
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if _, present := onDisk["codexCustomEnv"]; present {
		t.Fatalf("codexCustomEnv survived the last delete: %s", raw)
	}
}

func TestSetProviderEnvVarRejectsMalformedNames(t *testing.T) {
	cases := []struct {
		name    string
		varName string
		want    string
	}{
		{name: "empty", varName: "   ", want: "is empty"},
		{name: "equals sign", varName: "FOO=BAR", want: "must not contain"},
		{name: "leading digit", varName: "1FOO", want: "invalid character"},
		{name: "hyphen", varName: "MY-VAR", want: "invalid character"},
		{name: "space", varName: "MY VAR", want: "invalid character"},
		{name: "too long", varName: strings.Repeat("A", MaxProviderEnvNameLength+1), want: "max is"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			svc := NewService(t.TempDir())
			_, err := svc.SetProviderEnvVar("claude", test.varName, "v", false)
			if err == nil {
				t.Fatalf("SetProviderEnvVar(%q) succeeded, want rejection", test.varName)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to mention %q", err, test.want)
			}
		})
	}
}

// Every name Agent Overflow pins itself is refused at save time with a reason.
// Dropping one silently at spawn is the failure mode this replaces: the user
// would see their variable listed in Settings and never take effect.
func TestSetProviderEnvVarRejectsReservedNames(t *testing.T) {
	cases := []struct {
		provider string
		varName  string
	}{
		{"claude", "PATH"},
		{"claude", "path"},
		{"claude", "CLAUDE_CONFIG_DIR"},
		{"claude", "CLAUDE_CODE_ENTRYPOINT"},
		{"claude", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"},
		{"claude", "CLAUDE_CODE_AUTO_COMPACT_WINDOW"},
		// Reserved for the resume-cursor mirror's benefit, not because AO
		// sets it: under this variable the CLI drops rows the mirror still
		// counts on, and the thread fails to resume at all.
		{"claude", "CLAUDE_CODE_RESUME_INTERRUPTED_TURN"},
		{"claude-tui", "CLAUDE_CODE_RESUME_INTERRUPTED_TURN"},
		{"claude", "AO_TOKEN"},
		{"claude", "AO_ANYTHING_AT_ALL"},
		{"claude", "ao_token"},
		{"codex", "PATH"},
		{"codex", "CODEX_HOME"},
		{"codex", "AO_ENDPOINT"},
	}
	for _, test := range cases {
		t.Run(test.provider+"/"+test.varName, func(t *testing.T) {
			svc := NewService(t.TempDir())
			_, err := svc.SetProviderEnvVar(test.provider, test.varName, "v", false)
			if err == nil {
				t.Fatalf("%s accepted reserved name %q", test.provider, test.varName)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("error = %v, want it to say the name is reserved", err)
			}
		})
	}
}

// A provider's reservation is its own: CODEX_HOME means nothing to Claude, and
// reserving it there would block a legitimate configuration for no reason.
func TestReservedNamesAreScopedToTheirProvider(t *testing.T) {
	svc := NewService(t.TempDir())
	if _, err := svc.SetProviderEnvVar("claude", "CODEX_HOME", "/tmp/x", false); err != nil {
		t.Fatalf("claude should accept CODEX_HOME: %v", err)
	}
	if _, err := svc.SetProviderEnvVar("codex", "CLAUDE_CONFIG_DIR", "/tmp/x", false); err != nil {
		t.Fatalf("codex should accept CLAUDE_CONFIG_DIR: %v", err)
	}
}

// ANTHROPIC_BASE_URL is the feature's primary use case. claude-tui pins the
// child's copy, but that pin is routed to the gateway upstream instead of
// reserving the name — see provider.ReservedEnvNames.
func TestBaseURLIsNotReserved(t *testing.T) {
	svc := NewService(t.TempDir())
	if _, err := svc.SetProviderEnvVar("claude", "ANTHROPIC_BASE_URL", "https://gw.test", false); err != nil {
		t.Fatalf("ANTHROPIC_BASE_URL must stay configurable: %v", err)
	}
}

func TestValidateProviderEnvVarsRejectsDuplicates(t *testing.T) {
	cases := []struct {
		name string
		vars []ProviderEnvVar
		want string
	}{
		{
			name: "exact duplicate",
			vars: []ProviderEnvVar{{Name: "HTTPS_PROXY", Value: "a"}, {Name: "HTTPS_PROXY", Value: "b"}},
			want: "listed twice",
		},
		{
			name: "case-only duplicate",
			vars: []ProviderEnvVar{{Name: "HttpsProxy", Value: "a"}, {Name: "HTTPSPROXY", Value: "b"}},
			want: "differ only in case",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateProviderEnvVars("claude", test.vars); err == nil {
				t.Fatalf("duplicates accepted: %v", test.vars)
			} else if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to mention %q", err, test.want)
			}
		})
	}
}

func TestValidateProviderEnvVarsRejectsOversizeInput(t *testing.T) {
	tooMany := make([]ProviderEnvVar, MaxProviderEnvVars+1)
	for i := range tooMany {
		tooMany[i] = ProviderEnvVar{Name: "VAR_" + string(rune('A'+i%26)) + string(rune('0'+i/26)), Value: "v"}
	}
	if _, err := validateProviderEnvVars("claude", tooMany); err == nil {
		t.Fatal("a list past the cap was accepted")
	}
	long := []ProviderEnvVar{{Name: "BIG", Value: strings.Repeat("x", MaxProviderEnvValueLength+1)}}
	if _, err := validateProviderEnvVars("claude", long); err == nil {
		t.Fatal("an oversize value was accepted")
	}
	nul := []ProviderEnvVar{{Name: "BAD", Value: "a\x00b"}}
	if _, err := validateProviderEnvVars("claude", nul); err == nil {
		t.Fatal("a NUL-bearing value was accepted")
	}
}

// Values are stored verbatim: a trailing separator or a leading space can be
// meaningful to the CLI reading it, and trimming would silently change it.
func TestProviderEnvValuesAreNotTrimmed(t *testing.T) {
	svc := NewService(t.TempDir())
	got := mustSetEnvVar(t, svc, "claude", "NO_PROXY", "  a, b  ", false)
	if got.ClaudeCustomEnv[0].Value != "  a, b  " {
		t.Fatalf("value = %q, want it stored verbatim", got.ClaudeCustomEnv[0].Value)
	}
	// An empty value is legal — `FOO=` is a real thing to want.
	got = mustSetEnvVar(t, svc, "claude", "EMPTY_OK", "", false)
	if len(got.ClaudeCustomEnv) != 2 {
		t.Fatalf("ClaudeCustomEnv = %v, want the empty-valued entry kept", got.ClaudeCustomEnv)
	}
}

// The generic patch path must stay closed: it merges by wholesale assignment,
// so a GetSettings -> mutate -> Update round trip would write the redacted
// (empty) sensitive values back over the real ones.
func TestUpdateRejectsCustomEnvPatchKeys(t *testing.T) {
	svc := NewService(t.TempDir())
	for _, key := range []string{"claudeCustomEnv", "codexCustomEnv"} {
		_, err := svc.Update(map[string]any{key: []ProviderEnvVar{{Name: "X", Value: "1"}}})
		if err == nil {
			t.Fatalf("Update accepted the %s key", key)
		}
		if !strings.Contains(err.Error(), "SetProviderEnvVar") {
			t.Fatalf("error = %v, want it to name the dedicated mutators", err)
		}
	}
}

// Load-time is lenient where save-time is strict: one bad hand-edited entry
// must not strand the variables around it.
func TestSanitizeDropsInvalidEntriesOnLoad(t *testing.T) {
	dir := t.TempDir()
	raw := `{
	  "claudeCustomEnv": [
	    {"name": "GOOD", "value": "1"},
	    {"name": "BAD-NAME", "value": "2"},
	    {"name": "CLAUDE_CONFIG_DIR", "value": "/tmp/hijack"},
	    {"name": "AO_TOKEN", "value": "stolen"},
	    {"name": "good", "value": "dup"},
	    {"name": "ALSO_GOOD", "value": "3", "sensitive": true}
	  ]
	}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got := NewService(dir).Get()
	assertEnvVars(t, got.ClaudeCustomEnv, []ProviderEnvVar{
		{Name: "GOOD", Value: "1"},
		{Name: "ALSO_GOOD", Value: "3", Sensitive: true},
	})
}

func TestRedactProviderEnvVarsClearsOnlySensitiveValues(t *testing.T) {
	stored := []ProviderEnvVar{
		{Name: "OPEN", Value: "visible"},
		{Name: "SECRET", Value: "hunter2", Sensitive: true},
	}
	got := RedactProviderEnvVars(stored)
	assertEnvVars(t, got, []ProviderEnvVar{
		{Name: "OPEN", Value: "visible"},
		{Name: "SECRET", Value: "", Sensitive: true},
	})
	// The caller's slice (which aliases the service cache) is untouched.
	if stored[1].Value != "hunter2" {
		t.Fatalf("redaction mutated its input: %v", stored)
	}
}

func TestProviderEnvMapAndClaudeTUISharesClaudesList(t *testing.T) {
	svc := NewService(t.TempDir())
	current := mustSetEnvVar(t, svc, "claude", "ANTHROPIC_BASE_URL", "https://gw.test", false)

	want := map[string]string{"ANTHROPIC_BASE_URL": "https://gw.test"}
	for _, providerName := range []string{"claude", "claude-tui"} {
		got := current.ProviderEnvMap(providerName)
		if len(got) != 1 || got["ANTHROPIC_BASE_URL"] != want["ANTHROPIC_BASE_URL"] {
			t.Fatalf("ProviderEnvMap(%q) = %v, want %v", providerName, got, want)
		}
	}
	if got := current.ProviderEnvMap("codex"); got != nil {
		t.Fatalf("ProviderEnvMap(codex) = %v, want nil", got)
	}
	// An unknown provider reads as "nothing configured" rather than panicking:
	// spawn paths must keep working for a provider without the feature.
	if got := current.ProviderEnvMap("mystery"); got != nil {
		t.Fatalf("ProviderEnvMap(mystery) = %v, want nil", got)
	}
	if _, err := svc.SetProviderEnvVar("mystery", "X", "1", false); err == nil {
		t.Fatal("writing to an unknown provider's environment should error")
	}
}

func mustSetEnvVar(t *testing.T, svc *Service, providerName, name, value string, sensitive bool) Settings {
	t.Helper()
	got, err := svc.SetProviderEnvVar(providerName, name, value, sensitive)
	if err != nil {
		t.Fatalf("SetProviderEnvVar(%q, %q) error = %v", providerName, name, err)
	}
	return got
}

func assertEnvVars(t *testing.T, got, want []ProviderEnvVar) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("env vars = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("env var[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
