package provideraccountapp

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/testutil"
)

func TestAccountOperationsRespectRestartAdmission(t *testing.T) {
	for _, operation := range []string{"login", "switch", "remove", "refresh", "probe", "reconcile", "transfer"} {
		t.Run(operation, func(t *testing.T) {
			manager, _, _ := newTestManager(t)
			blocked := errors.New("restart in progress")
			manager.deps.BeginWork = func(context.Context) (func(), error) { return nil, blocked }
			var err error
			switch operation {
			case "login":
				_, err = manager.StartProviderLogin("claude", LoginMethodRemote)
			case "switch":
				_, err = manager.SwitchProviderAccount("claude", "saved")
			case "remove":
				err = manager.RemoveProviderAccount("claude", "saved")
			case "refresh":
				err = manager.RefreshProviderAccountUsage("claude", "saved")
			case "probe":
				_, err = manager.RunAccountProbe(ProbeRequest{ProviderName: "claude", Probe: func(context.Context) (provider.AccountInfo, error) {
					t.Fatal("provider probe crossed the restart fence")
					return provider.AccountInfo{}, nil
				}})
			case "reconcile":
				err = manager.ReconcileExternalProviderAccount("claude")
			case "transfer":
				err = manager.CheckCodexTransferAccount(t.Context(), "must-not-run")
			}
			if !errors.Is(err, blocked) {
				t.Fatalf("operation crossed restart fence: %v", err)
			}
		})
	}
}

func TestLoginAdmissionOutlivesStartAndReleasesAfterCancel(t *testing.T) {
	manager, store, _ := newTestManager(t)
	manager.deps.Context = t.Context
	if _, err := store.UpsertAndActivate(provideraccounts.Account{ID: "saved", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	binary := testutil.WriteMockCodexSession(t, t.TempDir(), map[string]string{
		"initialize":           `{"id":%s,"result":{"userAgent":"codex_cli_rs/0.151.0 (fake)"}}`,
		"account/login/start":  `{"id":%s,"result":{"type":"chatgptDeviceCode","loginId":"login-device","verificationUrl":"https://chatgpt.com/device","userCode":"ABCD-EFGH"}}`,
		"account/login/cancel": `{"id":%s,"result":{"status":"canceled"}}`,
	})
	manager.deps.ProviderBinary = func(string) string { return binary }
	var active atomic.Int32
	released := make(chan struct{}, 2)
	manager.deps.BeginWork = func(context.Context) (func(), error) {
		active.Add(1)
		return func() { active.Add(-1); released <- struct{}{} }, nil
	}
	t.Cleanup(manager.ShutdownProviderLogins)
	state, err := manager.StartProviderLogin("codex", LoginMethodRemote)
	if err != nil || state.Phase != LoginPhaseAwaitingCode {
		t.Fatalf("start: %+v, %v", state, err)
	}
	if active.Load() != 1 {
		t.Fatal("returning the sign-in link released the restart lease")
	}
	manager.CancelProviderLogin("codex")
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled sign-in retained its restart lease")
	}
	if active.Load() != 0 {
		t.Fatalf("active admissions after cleanup: %d", active.Load())
	}
	// A start that fails before the session driver launches also releases.
	missing := filepath.Join(t.TempDir(), "missing-provider")
	manager.deps.ProviderBinary = func(string) string { return missing }
	if _, err := manager.StartProviderLogin("codex", LoginMethodRemote); err == nil {
		t.Fatal("missing provider unexpectedly started")
	}
	if active.Load() != 0 {
		t.Fatalf("failed start retained %d admissions", active.Load())
	}
}
