package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provider/codex/codexfake"
)

// The mock's earned resets, driven through the production probe and spend so
// the mock's answers are read by AO's own decoder.

func codexResetMock(t *testing.T) (codex.ProbeConfig, codexfake.Store) {
	t.Helper()
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex-home")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"),
		[]byte(`{"tokens":{"access_token":"a","refresh_token":"r","account_id":"mock-workspace"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return codex.ProbeConfig{
		Reaper:  procutiltest.Reaper(),
		Binary:  mockBin,
		WorkDir: t.TempDir(),
		Env: provider.EnvInput{
			Custom: map[string]string{"HOME": home},
			Pins:   map[provider.ReservedEnv]string{provider.CodexHomeEnv: codexHome},
		},
	}, codexfake.Store{Home: home}
}

// spend sends key with creditID through the production spend, admitting the
// mock's login.
func spend(t *testing.T, cfg codex.ProbeConfig, key, creditID string) (string, error) {
	t.Helper()
	cfg.SendBy = time.Now().Add(time.Minute)
	cfg.BeforeConsume = func(provider.AccountInfo) (codex.ResetSpend, error) {
		return codex.ResetSpend{IdempotencyKey: key, CreditID: creditID}, nil
	}
	outcome, _, err := codex.SpendReset(t.Context(), cfg)
	return outcome, err
}

func readMockResets(t *testing.T, cfg codex.ProbeConfig) *codex.ResetCredits {
	t.Helper()
	var credits *codex.ResetCredits
	cfg.OnResetCredits = func(got *codex.ResetCredits) { credits = got }
	if _, err := codex.ProbeAccount(t.Context(), cfg); err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	return credits
}

func TestCodexMockReportsNoResetsUntilTheHarnessSeedsThem(t *testing.T) {
	cfg, _ := codexResetMock(t)
	if credits := readMockResets(t, cfg); credits != nil {
		t.Fatalf("an unseeded account reported %+v", credits)
	}
}

func TestCodexMockSpendsTheNamedResetOncePerKey(t *testing.T) {
	cfg, store := codexResetMock(t)
	if err := store.Set("mock-workspace", &codexfake.Account{Credits: []codexfake.Credit{
		{ID: "RateLimitResetCredit_late", Status: "available", GrantedAt: 1, ExpiresAt: 3000, Title: "Full reset"},
		{ID: "RateLimitResetCredit_soon", Status: "available", GrantedAt: 1, ExpiresAt: 2000},
	}}); err != nil {
		t.Fatal(err)
	}
	if credits := readMockResets(t, cfg); credits == nil || credits.AvailableCount != 2 || len(credits.Credits) != 2 {
		t.Fatalf("seeded read = %+v", credits)
	}

	var after *codex.ResetCredits
	spendCfg := cfg
	spendCfg.OnResetCredits = func(got *codex.ResetCredits) { after = got }
	outcome, err := spend(t, spendCfg, "key-1", "RateLimitResetCredit_soon")
	if err != nil || outcome != codex.ResetOutcomeReset {
		t.Fatalf("first spend = %q, %v", outcome, err)
	}
	if after == nil || after.AvailableCount != 1 || after.Credits[0].ID != "RateLimitResetCredit_late" {
		t.Fatalf("the read after the spend = %+v, want only the later credit left", after)
	}

	// The same key again spends nothing more.
	outcome, err = spend(t, cfg, "key-1", "RateLimitResetCredit_soon")
	if err != nil || outcome != codex.ResetOutcomeAlreadyRedeemed {
		t.Fatalf("repeat spend = %q, %v", outcome, err)
	}
	account, err := store.Get("mock-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if account.Consumes != 2 || len(account.Redeemed) != 1 || account.Redeemed["key-1"] != "RateLimitResetCredit_soon" {
		t.Fatalf("fake after two spends of one key = %+v", account)
	}

	// A named credit that is not available spends nothing.
	outcome, err = spend(t, cfg, "key-2", "RateLimitResetCredit_soon")
	if err != nil || outcome != codex.ResetOutcomeNoCredit {
		t.Fatalf("spend of a redeemed credit = %q, %v", outcome, err)
	}
}

// AO names a credit on every consume, and the mock answers no other.
func TestCodexMockRefusesAConsumeThatNamesNoCredit(t *testing.T) {
	_, store := codexResetMock(t)
	if err := store.Set("mock-workspace", &codexfake.Account{Credits: []codexfake.Credit{
		{ID: "RateLimitResetCredit_1", Status: "available", GrantedAt: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Consume("mock-workspace", "key-1", ""); err == nil {
		t.Fatal("the fake spent a consume that named no credit")
	}
	account, err := store.Get("mock-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if account.Credits[0].Status != "available" || account.Consumes != 0 {
		t.Fatalf("fake after an unnamed consume = %+v", account)
	}
}

func TestCodexMockLosesTheAnswerOfAnArmedSpendAfterSpendingIt(t *testing.T) {
	cfg, store := codexResetMock(t)
	if err := store.Set("mock-workspace", &codexfake.Account{
		Credits:         []codexfake.Credit{{ID: "RateLimitResetCredit_1", Status: "available", GrantedAt: 1}},
		FailNextConsume: true,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := spend(t, cfg, "key-1", "RateLimitResetCredit_1")
	var unconfirmed *codex.ResetUnconfirmedError
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("armed spend err = %v, want unconfirmed", err)
	}
	outcome, err := spend(t, cfg, "key-1", "RateLimitResetCredit_1")
	if err != nil || outcome != codex.ResetOutcomeAlreadyRedeemed {
		t.Fatalf("retry with the same key = %q, %v; want alreadyRedeemed", outcome, err)
	}
}

func TestCodexMockReportsTheSeededWindowAndASpendRefillsIt(t *testing.T) {
	cfg, store := codexResetMock(t)
	full := 100
	if err := store.Set("mock-workspace", &codexfake.Account{
		Credits:     []codexfake.Credit{{ID: "RateLimitResetCredit_1", Status: "available", GrantedAt: 1}},
		UsedPercent: &full,
	}); err != nil {
		t.Fatal(err)
	}
	var used []float64
	cfg.OnSnapshot = func(snapshot provider.RateLimitsSnapshot) {
		for _, limit := range snapshot.Limits {
			if limit.WindowMins == 300 {
				used = append(used, limit.UsedPercent)
			}
		}
	}
	if _, err := codex.ProbeAccount(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if outcome, err := spend(t, cfg, "key-1", "RateLimitResetCredit_1"); err != nil || outcome != codex.ResetOutcomeReset {
		t.Fatalf("spend = %q, %v", outcome, err)
	}
	if len(used) != 2 || used[0] != 100 || used[1] != 0 {
		t.Fatalf("5-hour usage read before and after the spend = %v, want [100 0]", used)
	}
}

func TestCodexMockAnswersACountOnlySummary(t *testing.T) {
	cfg, store := codexResetMock(t)
	if err := store.Set("mock-workspace", &codexfake.Account{
		Credits:   []codexfake.Credit{{ID: "RateLimitResetCredit_1", Status: "available", GrantedAt: 1}},
		CountOnly: true,
	}); err != nil {
		t.Fatal(err)
	}
	credits := readMockResets(t, cfg)
	if credits == nil || credits.AvailableCount != 1 || credits.Credits != nil {
		t.Fatalf("count-only read = %+v", credits)
	}
}
