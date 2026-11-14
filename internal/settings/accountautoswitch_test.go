package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAccountAutoSwitchDefaults(t *testing.T) {
	got := NewService(t.TempDir()).Get()
	if got.AccountAutoSwitchStrategy != AccountAutoSwitchSoonestReset {
		t.Fatalf("AccountAutoSwitchStrategy = %q, want %q", got.AccountAutoSwitchStrategy, AccountAutoSwitchSoonestReset)
	}
	if got.AccountAutoSwitchThreshold != DefaultAccountAutoSwitchThreshold {
		t.Fatalf("AccountAutoSwitchThreshold = %d, want %d", got.AccountAutoSwitchThreshold, DefaultAccountAutoSwitchThreshold)
	}
	if got.AccountAutoContinue {
		t.Fatal("AccountAutoContinue default = true, want false")
	}
}

func TestAccountAutoSwitchSettingsRoundTrip(t *testing.T) {
	svc := NewService(t.TempDir())

	got, err := svc.Update(map[string]any{
		"accountAutoSwitchStrategy": " " + AccountAutoSwitchAccountOrder + " ",
		"accountAutoContinue":       true,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.AccountAutoSwitchStrategy != AccountAutoSwitchAccountOrder || !got.AccountAutoContinue {
		t.Fatalf("got strategy %q continue %v", got.AccountAutoSwitchStrategy, got.AccountAutoContinue)
	}
	for _, threshold := range []int{MinAccountAutoSwitchThreshold, 80, MaxAccountAutoSwitchThreshold} {
		got, err = svc.Update(map[string]any{"accountAutoSwitchThreshold": threshold})
		if err != nil {
			t.Fatalf("accountAutoSwitchThreshold=%d: %v", threshold, err)
		}
		if got.AccountAutoSwitchThreshold != threshold {
			t.Fatalf("AccountAutoSwitchThreshold = %d, want %d", got.AccountAutoSwitchThreshold, threshold)
		}
	}
}

func TestAccountAutoSwitchSettingsRejectInvalidWrites(t *testing.T) {
	svc := NewService(t.TempDir())

	for _, threshold := range []int{0, MinAccountAutoSwitchThreshold - 1, MaxAccountAutoSwitchThreshold + 1} {
		if _, err := svc.Update(map[string]any{"accountAutoSwitchThreshold": threshold}); err == nil {
			t.Fatalf("accountAutoSwitchThreshold=%d: expected error, got nil", threshold)
		}
	}
	for _, strategy := range []string{"", "round-robin"} {
		if _, err := svc.Update(map[string]any{"accountAutoSwitchStrategy": strategy}); err == nil {
			t.Fatalf("accountAutoSwitchStrategy=%q: expected error, got nil", strategy)
		}
	}
	if got := svc.Get(); got.AccountAutoSwitchStrategy != AccountAutoSwitchSoonestReset ||
		got.AccountAutoSwitchThreshold != DefaultAccountAutoSwitchThreshold {
		t.Fatalf("rejected writes changed settings: %q %d", got.AccountAutoSwitchStrategy, got.AccountAutoSwitchThreshold)
	}
}

// A threshold outside the range is clamped to the nearest legal value; an
// unknown strategy falls back to the default.
func TestAccountAutoSwitchSettingsSanitizeOnLoad(t *testing.T) {
	for _, tc := range []struct {
		raw           string
		wantStrategy  string
		wantThreshold int
	}{
		{raw: `{"accountAutoSwitchThreshold": 0}`, wantStrategy: AccountAutoSwitchSoonestReset, wantThreshold: MinAccountAutoSwitchThreshold},
		{raw: `{"accountAutoSwitchThreshold": 49}`, wantStrategy: AccountAutoSwitchSoonestReset, wantThreshold: MinAccountAutoSwitchThreshold},
		{raw: `{"accountAutoSwitchThreshold": 101}`, wantStrategy: AccountAutoSwitchSoonestReset, wantThreshold: MaxAccountAutoSwitchThreshold},
		{raw: `{"accountAutoSwitchThreshold": 70}`, wantStrategy: AccountAutoSwitchSoonestReset, wantThreshold: 70},
		{raw: `{"accountAutoSwitchStrategy": "round-robin"}`, wantStrategy: AccountAutoSwitchSoonestReset, wantThreshold: DefaultAccountAutoSwitchThreshold},
		{raw: `{"accountAutoSwitchStrategy": "account-order"}`, wantStrategy: AccountAutoSwitchAccountOrder, wantThreshold: DefaultAccountAutoSwitchThreshold},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(tc.raw), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := NewService(dir).Get()
		if got.AccountAutoSwitchStrategy != tc.wantStrategy || got.AccountAutoSwitchThreshold != tc.wantThreshold {
			t.Fatalf("%s: got %q %d, want %q %d", tc.raw,
				got.AccountAutoSwitchStrategy, got.AccountAutoSwitchThreshold, tc.wantStrategy, tc.wantThreshold)
		}
	}
}
