package providerdiscoveryapp

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
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
			deps := Deps{Reaper: procutiltest.Reaper(), ProviderBinary: func(provider string) string {
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
			if err := s.CheckTransferReadiness(context.Background(), name); err != nil {
				t.Fatal(err)
			}
			accountError = errors.New("sign in first")
			if err := s.CheckTransferReadiness(context.Background(), name); !errors.Is(err, accountError) || calls != 2 {
				t.Fatal("cached readiness", calls, err)
			}
			status = "not_found"
			if err := s.CheckTransferReadiness(context.Background(), name); err == nil || calls != 2 {
				t.Fatal("probed missing binary", calls, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := s.CheckTransferReadiness(ctx, name); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

// TestTransferReadinessRefusesCodexBelowMinimum pins the receiver's refusal
// at the start of a transfer: the real detection runs the binary, and a
// Codex below the minimum or with an unreadable version is refused with the
// detection's message, before the account probe and before any file lands.
func TestTransferReadinessRefusesCodexBelowMinimum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell scripts require unix")
	}
	for _, tc := range []struct {
		output, want string
	}{
		{"codex-cli 0.153.4", "This computer cannot receive the conversation yet. Codex v0.153.4 (%s) is installed; Agent Overflow needs v0.154.0 or newer. Update Codex."},
		{"codex-cli dev", `This computer cannot receive the conversation yet. Agent Overflow could not read the version of Codex (%s) from "codex-cli dev". Update Codex to v0.154.0 or newer.`},
		{"codex-cli 0.154.0", ""},
	} {
		t.Run(tc.output, func(t *testing.T) {
			binary := mockexec.WriteIn(t, t.TempDir(), "codex", "#!/bin/sh\necho '"+tc.output+"'\n")
			calls := 0
			s := New(Deps{Reaper: procutiltest.Reaper(), ProviderBinary: func(string) string { return binary },
				DetectProvider:            provider.DetectProvider,
				CheckCodexTransferAccount: func(context.Context, string) error { calls++; return nil },
			}, testCaches())
			err := s.CheckTransferReadiness(context.Background(), "codex")
			if tc.want == "" {
				if err != nil || calls != 1 {
					t.Fatalf("supported Codex: %v, probes=%d", err, calls)
				}
				return
			}
			if err == nil || err.Error() != fmt.Sprintf(tc.want, binary) || calls != 0 {
				t.Fatalf("err = %v, probes=%d; want %q", err, calls, fmt.Sprintf(tc.want, binary))
			}
		})
	}
}
