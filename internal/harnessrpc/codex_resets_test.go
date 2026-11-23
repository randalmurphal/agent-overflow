package harnessrpc

import (
	"path/filepath"
	"testing"

	"agent-overflow/internal/provider/codex/codexfake"
)

func TestHarnessSetCodexResetCreditsSeedsReadsAndResetClears(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	spec := HarnessCodexResetCreditsSpec{AccountID: "mock-workspace", Account: &codexfake.Account{
		Credits: []codexfake.Credit{{ID: "RateLimitResetCredit_1", Status: "available", GrantedAt: 1}},
	}}
	if err := h.HarnessSetCodexResetCredits(spec); err == nil {
		t.Fatal("a boot without the harness home accepted a seed")
	}

	h.config.HomeDir = filepath.Join(t.TempDir(), "home")
	if err := h.HarnessSetCodexResetCredits(spec); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := h.HarnessCodexResetCredits("mock-workspace")
	if err != nil || got == nil || len(got.Credits) != 1 {
		t.Fatalf("read back = %+v, %v", got, err)
	}
	if err := h.resetCodexResetCredits(); err != nil {
		t.Fatal(err)
	}
	if got, err := h.HarnessCodexResetCredits("mock-workspace"); err != nil || got != nil {
		t.Fatalf("after reset = %+v, %v; want none", got, err)
	}
	// A reset with nothing seeded is not an error.
	if err := h.resetCodexResetCredits(); err != nil {
		t.Fatal(err)
	}
}
