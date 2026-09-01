package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// initializeModelsFixture is the real 2.1.219 `initialize` control_response,
// trimmed to the keys this package reads (the commands/agents arrays and the
// TUI render payload are dropped; the account identity is anonymised). Kept as
// a captured envelope rather than a hand-written literal so a wire change
// shows up as a test failure instead of as agreement with our own guess.
const initializeModelsFixture = "../../../docs/references/fixtures/claude/initialize_models_20260802.json"

func loadInitializeFixtureLine(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(initializeModelsFixture))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// The fixture is stored pretty-printed for review; the wire is NDJSON.
	var line bytes.Buffer
	if err := json.Compact(&line, raw); err != nil {
		t.Fatalf("compact fixture: %v", err)
	}
	return line.Bytes()
}

// TestParseInitResponseReadsCapturedModels is the fixture-backed shape test:
// every field the merge policy depends on, read off the real capture.
func TestParseInitResponseReadsCapturedModels(t *testing.T) {
	parsed, matched, err := tryParseControlInitResponse(loadInitializeFixtureLine(t))
	if err != nil || !matched {
		t.Fatalf("tryParseControlInitResponse: matched=%v err=%v", matched, err)
	}
	if parsed.ModelsErr != nil {
		t.Fatalf("ModelsErr = %v, want nil", parsed.ModelsErr)
	}
	if parsed.Account.SubscriptionType != "Claude Max" {
		t.Errorf("account rides on the same response: SubscriptionType = %q", parsed.Account.SubscriptionType)
	}
	if len(parsed.Models) != 5 {
		t.Fatalf("len(Models) = %d, want the 5 picker rows", len(parsed.Models))
	}

	want := []struct {
		value         string
		canonical     string
		extended      bool
		fastMode      bool
		effortLevels  int
		supportEffort bool
	}{
		{value: "default", canonical: "claude-opus-5", extended: true, fastMode: true, effortLevels: 5, supportEffort: true},
		{value: "opus[1m]", canonical: "claude-opus-5", extended: true, fastMode: true, effortLevels: 5, supportEffort: true},
		// The wire's one inconsistency: `[1m]` on the alias, absent from the
		// id it resolves to. Both sides must normalise to the same slug.
		{value: "claude-fable-5[1m]", canonical: "claude-fable-5", extended: true, effortLevels: 5, supportEffort: true},
		{value: "sonnet", canonical: "claude-sonnet-5", effortLevels: 5, supportEffort: true},
		// Haiku carries no effort fields at all — the discrepancy against the
		// catalog that the 2.7 spike found.
		{value: "haiku", canonical: "claude-haiku-4-5"},
	}

	for i, expected := range want {
		row := parsed.Models[i]
		if row.Value != expected.value {
			t.Errorf("row %d Value = %q, want %q", i, row.Value, expected.value)
		}
		if got := row.CanonicalSlug(); got != expected.canonical {
			t.Errorf("row %d CanonicalSlug = %q, want %q", i, got, expected.canonical)
		}
		if got := row.DeclaresExtendedContext(); got != expected.extended {
			t.Errorf("row %d DeclaresExtendedContext = %v, want %v", i, got, expected.extended)
		}
		if row.SupportsFastMode != expected.fastMode {
			t.Errorf("row %d SupportsFastMode = %v, want %v", i, row.SupportsFastMode, expected.fastMode)
		}
		if row.SupportsEffort != expected.supportEffort {
			t.Errorf("row %d SupportsEffort = %v, want %v", i, row.SupportsEffort, expected.supportEffort)
		}
		if len(row.SupportedEffortLevels) != expected.effortLevels {
			t.Errorf("row %d effort levels = %d, want %d", i, len(row.SupportedEffortLevels), expected.effortLevels)
		}
		if row.DisplayName == "" {
			t.Errorf("row %d has no display name", i)
		}
	}

	// supportsAutoMode is three-state on decode. The capture proves why:
	// the Haiku row OMITS the key, and the old bool decode read that as
	// an explicit "false" — a manufactured denial. Absent must stay nil;
	// the merge restricts Auto only on explicit denials (see
	// internal/claudemodels/AGENTS.md).
	if v := parsed.Models[0].SupportsAutoMode; v == nil || !*v {
		t.Errorf("Opus row: SupportsAutoMode = %v, want explicit true", v)
	}
	if v := parsed.Models[4].SupportsAutoMode; v != nil {
		t.Errorf("Haiku row: SupportsAutoMode = %v, want nil — the capture omits the key", *v)
	}
}

func TestCanonicalSlugNormalizations(t *testing.T) {
	tests := []struct {
		name string
		row  WireModel
		want string
	}{
		{
			name: "resolved id wins over the alias",
			row:  WireModel{Value: "sonnet", ResolvedModel: "claude-sonnet-5"},
			want: "claude-sonnet-5",
		},
		{
			name: "context marker stripped from the resolved id",
			row:  WireModel{Value: "opus[1m]", ResolvedModel: "claude-opus-5[1m]"},
			want: "claude-opus-5",
		},
		{
			name: "context marker stripped from a bare value",
			row:  WireModel{Value: "claude-fable-5[1m]"},
			want: "claude-fable-5",
		},
		{
			name: "dated id folds onto the catalog slug",
			row:  WireModel{Value: "haiku", ResolvedModel: "claude-haiku-4-5-20251001"},
			want: "claude-haiku-4-5",
		},
		{
			name: "alias with no resolved id still normalises",
			row:  WireModel{Value: "opus"},
			want: "claude-opus-5",
		},
		{
			name: "unknown model passes through untouched",
			row:  WireModel{Value: "claude-opus-9[1m]"},
			want: "claude-opus-9",
		},
		{
			name: "empty row names nothing",
			row:  WireModel{},
			want: "",
		},
		{
			name: "a bare marker is not a model id",
			row:  WireModel{Value: "[1m]"},
			want: "[1m]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.row.CanonicalSlug(); got != tt.want {
				t.Errorf("CanonicalSlug() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecodeWireModelsDistinguishesAbsentFromUnreadable pins the two answers
// the OnModels contract is built on: an absent array is a real answer (nil,
// nil), an unreadable one is no answer at all (nil, err).
func TestDecodeWireModelsDistinguishesAbsentFromUnreadable(t *testing.T) {
	models, err := decodeWireModels([]byte(`{"account":{}}`))
	if err != nil || models != nil {
		t.Errorf("absent array: got (%v, %v), want (nil, nil)", models, err)
	}
	models, err = decodeWireModels([]byte(`{"models":[]}`))
	if err != nil || len(models) != 0 {
		t.Errorf("empty array: got (%v, %v), want (empty, nil)", models, err)
	}
	if _, err := decodeWireModels([]byte(`{"models":"soon"}`)); err == nil {
		t.Error("unreadable array: got nil error, want a decode failure")
	}
}

// TestProbeAccountReportsModelsAndAccountFromOneResponse proves the enrichment
// costs no second subprocess: one probe, one initialize response, both answers.
func TestProbeAccountReportsModelsAndAccountFromOneResponse(t *testing.T) {
	line := loadInitializeFixtureLine(t)
	parsed, matched, err := tryParseControlInitResponse(line)
	if err != nil || !matched {
		t.Fatalf("tryParseControlInitResponse: matched=%v err=%v", matched, err)
	}
	if parsed.Account.APIProvider != "firstParty" {
		t.Errorf("APIProvider = %q, want firstParty", parsed.Account.APIProvider)
	}
	if len(parsed.Models) == 0 {
		t.Error("models must ride on the same response the account came from")
	}
}

// TestInitResponseModelDecodeFailureDoesNotFailTheProbe pins the split error
// contract: identity survives a malformed cosmetic sub-field.
func TestInitResponseModelDecodeFailureDoesNotFailTheProbe(t *testing.T) {
	line := []byte(`{"type":"control_response","response":{"subtype":"success",` +
		`"request_id":"` + probeInitRequestID + `","response":{` +
		`"account":{"subscriptionType":"Claude Max"},"models":"soon"}}}`)

	parsed, matched, err := tryParseControlInitResponse(line)
	if err != nil {
		t.Fatalf("a malformed models array must not fail the probe: %v", err)
	}
	if !matched {
		t.Fatal("matched = false, want the response to be recognised")
	}
	if parsed.Account.SubscriptionType != "Claude Max" {
		t.Errorf("SubscriptionType = %q, want Claude Max", parsed.Account.SubscriptionType)
	}
	if parsed.ModelsErr == nil {
		t.Error("ModelsErr = nil, want the decode failure surfaced")
	}
	if parsed.Models != nil {
		t.Errorf("Models = %v, want nil alongside an error", parsed.Models)
	}
}
