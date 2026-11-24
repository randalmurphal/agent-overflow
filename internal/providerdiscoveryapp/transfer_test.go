package providerdiscoveryapp

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerstatus"
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

// TestTransferReadinessLeavesTheCodexVersionToTheAccountHandshake pins
// that `--version` output, which runs in AO's own environment, does not
// decide an incoming Codex transfer: below the minimum, unreadable or
// supported, the account probe runs, and its handshake (started with the
// configured environment) admits or refuses the build.
func TestTransferReadinessLeavesTheCodexVersionToTheAccountHandshake(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell scripts require unix")
	}
	for _, output := range []string{"codex-cli 0.153.4", "codex-cli dev", "codex-cli 0.154.0"} {
		t.Run(output, func(t *testing.T) {
			binary := mockexec.WriteIn(t, t.TempDir(), "codex", "#!/bin/sh\necho '"+output+"'\n")
			calls, emitted := 0, 0
			refusal := errors.New("the handshake's answer")
			s := New(Deps{Reaper: procutiltest.Reaper(), ProviderBinary: func(string) string { return binary },
				DetectProvider:            provider.DetectProvider,
				CheckCodexTransferAccount: func(context.Context, string) error { calls++; return refusal },
				EmitStatus:                func(providerstatus.Event) { emitted++ },
			}, testCaches())
			err := s.CheckTransferReadiness(context.Background(), "codex")
			if !errors.Is(err, refusal) || calls != 1 || emitted != 0 {
				t.Fatalf("err = %v, probes = %d, status events = %d; want the account handshake's answer alone",
					err, calls, emitted)
			}
		})
	}
}
