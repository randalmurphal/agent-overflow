package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseValidFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/valid/profile.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if result := Validate(got); !result.Valid() {
		t.Fatalf("Validate findings = %+v", result.Findings)
	}
	if got.BaseBranch != "main" || got.Disposition != DispositionAutoPR {
		t.Fatalf("profile defaults/scalars = %+v", got)
	}
	if !got.HasCheck("test") || !got.HasCapacity("live-stack") || !got.HasCommand("report-issue") {
		t.Fatalf("profile does not expose expected bindings: %+v", got)
	}
	if got.HasCheck("missing") || got.HasCapacity("missing") || got.HasCommand("missing") {
		t.Fatal("profile reports an undeclared binding")
	}
}

func TestDefaultAndNilBindings(t *testing.T) {
	got := Default()
	if got.Disposition != DispositionManual || got.Checks != nil || got.Capacities != nil || got.Commands != nil || got.Secrets != nil ||
		!reflect.DeepEqual(got.Reliability.Backoff, DefaultBackoff()) {
		t.Fatalf("Default = %+v", got)
	}
	var nilProfile *Profile
	if nilProfile.HasCheck("x") || nilProfile.HasCapacity("x") || nilProfile.HasCommand("x") {
		t.Fatal("nil profile exposes bindings")
	}
}

func TestParseAppliesDefaultBackoffWhenAbsent(t *testing.T) {
	got, err := ParseBytes([]byte("reliability:\n  watchdog: 1m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Reliability.Backoff, DefaultBackoff()) {
		t.Fatalf("backoff = %v, want %v", got.Reliability.Backoff, DefaultBackoff())
	}

	explicit, err := ParseBytes([]byte("reliability:\n  backoff: [1ms, 2ms]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []Duration{"1ms", "2ms"}; !reflect.DeepEqual(explicit.Reliability.Backoff, want) {
		t.Fatalf("explicit backoff = %v, want %v", explicit.Reliability.Backoff, want)
	}

	empty, err := ParseBytes([]byte("reliability:\n  backoff: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if result := Validate(empty); result.Valid() || len(result.Findings) != 1 || result.Findings[0].Code != "reliability.backoff" {
		t.Fatalf("empty backoff findings = %+v", result.Findings)
	}
}

func TestValidationFindingsGolden(t *testing.T) {
	profile := Profile{
		BaseBranch:  "   ",
		Checks:      map[string][]string{"Bad_Check": {}},
		Capacities:  map[string]int{"Bad_Capacity": 0},
		Commands:    map[string][]string{"Bad_Command": {" "}},
		Disposition: "ship-it",
		Reliability: ReliabilityDefaults{
			Watchdog: "eventually",
			Backoff:  []Duration{"0s", "later"},
			PerItemBudget: &Budget{
				Tokens:    pointer(int64(-1)),
				USD:       pointer(math.NaN()),
				WallClock: pointer(Duration("-1s")),
			},
		},
		Secrets: map[string]Secret{
			"Bad_Source":  {Source: "keychain"},
			"env-fields":  {Source: "env", Path: "/wrong"},
			"file-fields": {Source: "file", Env: "WRONG"},
		},
		MCPServers: []string{"", "github", "github"},
		WorktreeSetup: WorktreeSetup{
			Copy: []string{" "},
			Run:  [][]string{{}},
		},
	}
	result := Validate(profile)
	if result.Valid() {
		t.Fatal("Validate unexpectedly succeeded")
	}
	var actual strings.Builder
	for _, finding := range result.Findings {
		fmt.Fprintf(&actual, "%s | %s | %s\n", finding.Code, finding.Element, finding.Message)
	}
	want, err := os.ReadFile("testdata/invalid/validation.golden")
	if err != nil {
		t.Fatal(err)
	}
	if actual.String() != string(want) {
		t.Fatalf("validation findings:\n%s\nwant:\n%s", actual.String(), want)
	}
}

func TestValidatePositiveReliabilityVariants(t *testing.T) {
	for name, budget := range map[string]*Budget{
		"tokens":     {Tokens: pointer(int64(1))},
		"usd":        {USD: pointer(0.01)},
		"wall-clock": {WallClock: pointer(Duration("2h30m"))},
	} {
		t.Run(name, func(t *testing.T) {
			got := Validate(Profile{
				Disposition: DispositionManual,
				Reliability: ReliabilityDefaults{Watchdog: "15m", Backoff: []Duration{"30s", "2m", "5m"}, PerItemBudget: budget},
			})
			if !got.Valid() {
				t.Fatalf("Validate findings = %+v", got.Findings)
			}
		})
	}
}

func TestValidateBudgetTracksAuthoredZeroFields(t *testing.T) {
	zero := int64(0)
	usd := 5.0
	result := Validate(Profile{
		Disposition: DispositionManual,
		Reliability: ReliabilityDefaults{PerItemBudget: &Budget{Tokens: &zero, USD: &usd}},
	})
	if len(result.Findings) != 2 || result.Findings[0].Code != "reliability.budget" || result.Findings[1].Code != "reliability.budget-tokens" {
		t.Fatalf("Validate findings = %+v", result.Findings)
	}
}

func TestValidateBudgetForWorkItemParameter(t *testing.T) {
	if result := ValidateBudget(nil); !result.Valid() {
		t.Fatalf("nil optional budget findings = %+v", result.Findings)
	}
	zero := int64(0)
	result := ValidateBudget(&Budget{Tokens: &zero})
	if result.Valid() || len(result.Findings) != 1 || result.Findings[0].Code != "reliability.budget-tokens" || result.Findings[0].Element != "work item budget.tokens" {
		t.Fatalf("invalid item budget findings = %+v", result.Findings)
	}
}

func TestLoadAbsentReturnsDefault(t *testing.T) {
	got, defaulted, err := Load(filepath.Join(t.TempDir(), "profile.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !defaulted || !reflect.DeepEqual(got, Default()) {
		t.Fatalf("Load = (%+v, %v), want default", got, defaulted)
	}
}

func TestLoadPresentNeverFallsBack(t *testing.T) {
	tests := map[string]string{
		"malformed":     "checks: [",
		"unknown-field": "unknown: true\n",
		"multiple-docs": "disposition: manual\n---\ndisposition: auto-pr\n",
		"invalid":       "capacities:\n  test: 0\n",
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile.yaml")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			got, defaulted, err := Load(path)
			if err == nil {
				t.Fatalf("Load = (%+v, %v, nil), want error", got, defaulted)
			}
			if defaulted {
				t.Fatal("present malformed profile was marked defaulted")
			}
		})
	}
}

func TestLoadRejectsOversizeProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.yaml")
	data := make([]byte, MaxProfileBytes+1)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, defaulted, err := Load(path)
	if err == nil || defaulted || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Load error = %v, defaulted = %v", err, defaulted)
	}
}

func TestResolveSecretsEnvAndFile(t *testing.T) {
	t.Setenv("PROFILE_TOKEN", "env-secret")
	path := filepath.Join(t.TempDir(), "secret")
	// The trailing newline is the conventional secret-file terminator; the
	// resolved value and mask must both drop it or masking never matches
	// the value as it appears in text.
	if err := os.WriteFile(path, []byte("file-secret\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (Profile{Secrets: map[string]Secret{
		"from-file": {Source: "file", Path: path},
		"from-env":  {Source: "env", Env: "PROFILE_TOKEN"},
	}}).ResolveSecrets()
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	wantValues := map[string]string{"from-env": "env-secret", "from-file": "file-secret"}
	wantMasks := []string{"env-secret", "file-secret"}
	if !reflect.DeepEqual(got.Values, wantValues) || !reflect.DeepEqual(got.Masks, wantMasks) {
		t.Fatalf("ResolveSecrets = %#v", got)
	}
	formatted := []string{fmt.Sprint(got), fmt.Sprintf("%+v", got), fmt.Sprintf("%#v", got)}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	formatted = append(formatted, string(encoded))
	for _, rendered := range formatted {
		if strings.Contains(rendered, "env-secret") || strings.Contains(rendered, "file-secret") {
			t.Fatalf("resolved secret value leaked in formatting: %s", rendered)
		}
	}
}

func TestResolveSecretsRejectsOversizedAndNonRegularFiles(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, make([]byte, MaxSecretFileBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := (Profile{Secrets: map[string]Secret{"token": {Source: "file", Path: path}}}).ResolveSecrets()
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("ResolveSecrets error = %v", err)
		}
	})
	t.Run("non-regular", func(t *testing.T) {
		_, err := (Profile{Secrets: map[string]Secret{"token": {Source: "file", Path: t.TempDir()}}}).ResolveSecrets()
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("ResolveSecrets error = %v", err)
		}
	})
}

func TestResolveSecretsTypedFailures(t *testing.T) {
	tests := map[string]Profile{
		"missing-env": {
			Secrets: map[string]Secret{"api-token": {Source: "env", Env: "PROFILE_TEST_UNSET"}},
		},
		"missing-file": {
			Secrets: map[string]Secret{"api-token": {Source: "file", Path: filepath.Join(t.TempDir(), "missing")}},
		},
	}
	for name, profile := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := profile.ResolveSecrets()
			var resolutionError *SecretResolutionError
			if !errors.As(err, &resolutionError) {
				t.Fatalf("ResolveSecrets error = %T %v", err, err)
			}
			if resolutionError.Secret != "api-token" {
				t.Fatalf("error secret = %q", resolutionError.Secret)
			}
		})
	}
}

func TestSecretErrorsNeverLeakResolvedValues(t *testing.T) {
	const planted = "PLANTED-SECRET-VALUE-9f2f"
	t.Setenv("PROFILE_PLANTED", planted)
	profile := Profile{Secrets: map[string]Secret{
		"a-resolved": {Source: "env", Env: "PROFILE_PLANTED"},
		"z-missing":  {Source: "env", Env: "PROFILE_MISSING"},
	}}
	_, err := profile.ResolveSecrets()
	if err == nil {
		t.Fatal("ResolveSecrets unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), planted) || strings.Contains(fmt.Sprint(err), planted) {
		t.Fatalf("secret value leaked in error: %v", err)
	}
}

func pointer[T any](value T) *T { return &value }
