package providerdiscoveryapp

import (
	"context"
	"errors"
	"testing"

	"agent-overflow/internal/provider"
)

func TestTransferReadinessChecksOnlySelectedProviderAndFreshAccount(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			status, calls := "ready", 0
			var accountError error
			check := func(_ context.Context, binary string) error {
				calls++
				if binary != "/mock/"+name {
					t.Fatal("wrong binary", binary)
				}
				return accountError
			}
			wrong := func(context.Context, string) error { t.Fatal("probed another provider"); return nil }
			deps := Deps{ProviderBinary: func(provider string) string {
				if provider != name {
					t.Fatal("wrong provider", provider)
				}
				return "/mock/" + name
			}, DetectProvider: func(providerName, binary string) provider.ProviderStatus {
				return provider.ProviderStatus{Provider: providerName, BinaryPath: binary, Status: status}
			}, CheckClaudeTransferAccount: wrong, CheckCodexTransferAccount: wrong}
			if name == "claude" {
				deps.CheckClaudeTransferAccount = check
			} else {
				deps.CheckCodexTransferAccount = check
			}
			s := New(deps, testCaches())
			if err := s.CheckTransferReadiness(context.Background(), name, ""); err != nil {
				t.Fatal(err)
			}
			accountError = errors.New("sign in first")
			if err := s.CheckTransferReadiness(context.Background(), name, ""); !errors.Is(err, accountError) || calls != 2 {
				t.Fatal("cached readiness", calls, err)
			}
			status = "not_found"
			if err := s.CheckTransferReadiness(context.Background(), name, ""); err == nil || calls != 2 {
				t.Fatal("probed missing binary", calls, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := s.CheckTransferReadiness(ctx, name, ""); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestTransferReadinessUsesNativeFormatFloorBeforeAccountProbe(t *testing.T) {
	for _, tc := range []struct {
		version, floor string
		ready          bool
	}{
		{"codex-cli 0.147.0", "0.148.0", false},
		{"codex-cli 0.148.0", "0.148.0", true},
		{"codex-cli 0.153.4", "0.148.0", true},
		{"codex-cli 0.147.0", "", true},
		{"unreadable version", "0.148.0", false},
	} {
		t.Run(tc.version+"/"+tc.floor, func(t *testing.T) {
			calls := 0
			s := New(Deps{ProviderBinary: func(string) string { return "mock" },
				DetectProvider: func(string, string) provider.ProviderStatus {
					return provider.ProviderStatus{Status: "ready", Version: tc.version}
				},
				CheckCodexTransferAccount: func(context.Context, string) error { calls++; return nil },
			}, testCaches())
			err := s.CheckTransferReadiness(context.Background(), "codex", tc.floor)
			if (err == nil) != tc.ready || (calls > 0) != tc.ready {
				t.Fatalf("format admission: %v, probes=%d", err, calls)
			}
		})
	}
}
