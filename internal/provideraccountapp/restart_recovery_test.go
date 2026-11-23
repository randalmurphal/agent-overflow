package provideraccountapp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/testutil/mockexec"
)

const (
	crashedRefreshUserHomeEnv = "AO_TEST_CRASHED_REFRESH_USER_HOME"
	crashedRefreshDBDirEnv    = "AO_TEST_CRASHED_REFRESH_DB_DIR"
	crashedRefreshBinaryEnv   = "AO_TEST_CRASHED_REFRESH_BINARY"
	crashedRefreshAccountEnv  = "AO_TEST_CRASHED_REFRESH_ACCOUNT"
)

// codexWorkspaceLogin is a ChatGPT-mode auth.json for one login of one
// workspace at one point on its rotation chain.
func codexWorkspaceLogin(t *testing.T, email, workspace, refreshToken string, lastRefresh time.Time) []byte {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		"email":                       email,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": workspace},
	})
	if err != nil {
		t.Fatal(err)
	}
	segment := base64.RawURLEncoding.EncodeToString
	data, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":      segment([]byte(`{"alg":"none"}`)) + "." + segment(claims) + "." + segment([]byte("sig")),
			"access_token":  "access-" + refreshToken,
			"refresh_token": refreshToken,
		},
		"last_refresh": lastRefresh.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// openBackend opens one backend's account store and credentials over a
// provider home, the way a boot does.
func openBackend(t *testing.T, userHome, dbDir, binary string) (*Manager, *provideraccounts.Store, *provideraccounts.Credentials) {
	t.Helper()
	store, err := provideraccounts.NewStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provideraccounts.NewCredentialsWithFileKeychain(userHome, CredentialPolicy())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(Deps{Reaper: procutiltest.Reaper(), ProviderBinary: func(string) string { return binary }})
	if err := manager.Attach(store, credentials, ""); err != nil {
		t.Fatal(err)
	}
	return manager, store, credentials
}

// TestCrashedSelectedUsageRefreshHelper is the backend a crash kills: it
// runs a manual usage refresh through production code and is killed while
// the probe runs. It runs only when crashSelectedUsageRefresh starts it.
//
// It starts with the canonical credential recorded as the selected
// account's, so the refresh seeds its home from it without reconciling
// first: the registry entries an earlier release left behind, which recorded
// an outside login that way and never reconciled before a refresh, are what
// a boot after the upgrade has to handle.
func TestCrashedSelectedUsageRefreshHelper(t *testing.T) {
	userHome := os.Getenv(crashedRefreshUserHomeEnv)
	if userHome == "" {
		t.Skip("helper process only")
	}
	manager, _, credentials := openBackend(t, userHome, os.Getenv(crashedRefreshDBDirEnv), os.Getenv(crashedRefreshBinaryEnv))
	canonical, err := credentials.ReadCredential(string(provider.Codex), "", true)
	if err != nil {
		t.Fatal(err)
	}
	// The previous release recorded whatever canonical credential it last
	// saw for the selected account; this build records only one it
	// identified as the account's, so the record is forced here.
	manager.BlessCredentialForTest(string(provider.Codex), canonical)
	err = manager.RefreshProviderAccountUsage(string(provider.Codex), os.Getenv(crashedRefreshAccountEnv))
	t.Fatalf("refresh returned before the kill: %v", err)
}

// crashSelectedUsageRefresh refreshes accountID's usage in a backend of its
// own over userHome and dbDir, and kills that backend and its Codex probe
// while the probe runs in its temporary home: after the probe put rotated
// there, when rotated is not nil. It returns the temporary home's path once
// both processes are dead.
func crashSelectedUsageRefresh(t *testing.T, userHome, dbDir, accountID string, rotated []byte) string {
	t.Helper()
	work := t.TempDir()
	rotateLine := ""
	if rotated != nil {
		rotatedFile := filepath.Join(work, "rotated.json")
		if err := os.WriteFile(rotatedFile, rotated, 0o600); err != nil {
			t.Fatal(err)
		}
		rotateLine = "cp '" + rotatedFile + "' \"$CODEX_HOME/auth.json\"\n"
	}
	marker := filepath.Join(work, "probe-running")
	binary := mockexec.Write(t, filepath.Join(work, "mock-codex"), "#!/bin/bash\n"+
		rotateLine+
		"printf '%s %s' \"$$\" \"$CODEX_HOME\" > '"+marker+".tmp'\n"+
		"mv '"+marker+".tmp' '"+marker+"'\n"+
		"exec sleep 60\n")

	helper := exec.Command(os.Args[0], "-test.run=^TestCrashedSelectedUsageRefreshHelper$", "-test.count=1")
	helper.Env = append(os.Environ(),
		crashedRefreshUserHomeEnv+"="+userHome,
		crashedRefreshDBDirEnv+"="+dbDir,
		crashedRefreshBinaryEnv+"="+binary,
		crashedRefreshAccountEnv+"="+accountID,
	)
	var output strings.Builder
	helper.Stdout, helper.Stderr = &output, &output
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	killHelper := func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	}
	var report []byte
	for deadline := time.Now().Add(20 * time.Second); ; {
		data, err := os.ReadFile(marker)
		if err == nil {
			report = data
			break
		}
		if time.Now().After(deadline) {
			killHelper()
			t.Fatalf("the refresh never started its probe:\n%s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	killHelper()
	pidText, homePath, _ := strings.Cut(string(report), " ")
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatalf("probe report %q: %v", report, err)
	}
	if probe, err := os.FindProcess(pid); err == nil {
		_ = probe.Kill()
	}
	t.Cleanup(func() { _ = os.RemoveAll(homePath) })
	return homePath
}

// asPreviousReleaseEntries rewrites the Codex registry entries under userHome
// into the shape the previous release wrote: without the token the owner's
// slot held at seeding. That release could seed a selected account's home
// from another login's canonical credential, so only its entries carry such
// a seed.
func asPreviousReleaseEntries(t *testing.T, userHome string) {
	t.Helper()
	dir := filepath.Join(userHome, ".codex", "agent-overflow-ephemerals")
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("registry entries in %s: %v, %v", dir, paths, err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var entry map[string]any
		if err := json.Unmarshal(data, &entry); err != nil {
			t.Fatal(err)
		}
		delete(entry, "slotToken")
		if data, err = json.Marshal(entry); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func mustReadCredential(t *testing.T, credentials *provideraccounts.Credentials, accountID string, active bool) string {
	t.Helper()
	data, err := credentials.ReadCredential(string(provider.Codex), accountID, active)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// An outside `codex login` as B replaced the canonical credential while A
// was selected, and a usage refresh of A died while its probe ran in a home
// seeded from that credential. However the probe ended (B's token rotated or
// not), whether B shares A's email, and whether this backend or another data
// root's sweeps first, no restart leaves B's login in A's slot. B's
// rotation goes back to the canonical credential it came from.
func TestRestartAfterACrashedSelectedRefreshKeepsEachLoginInItsSlot(t *testing.T) {
	for _, rotated := range []bool{true, false} {
		for _, email := range []string{"b@example.com", "same@example.com"} {
			for _, otherRootFirst := range []bool{false, true} {
				name := "rotated=" + strconv.FormatBool(rotated) + "/b=" + email + "/other_root_first=" + strconv.FormatBool(otherRootFirst)
				t.Run(name, func(t *testing.T) { testRestartAfterACrashedSelectedRefresh(t, rotated, email, otherRootFirst) })
			}
		}
	}
}

func testRestartAfterACrashedSelectedRefresh(t *testing.T, rotated bool, bEmail string, otherRootFirst bool) {
	kerneltest.IsolateSpawns(t)
	codexName := string(provider.Codex)
	userHome, dbDir := t.TempDir(), t.TempDir()
	aLogin := codexWorkspaceLogin(t, "same@example.com", "ws-a", "rt-a1", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	bSaved := codexWorkspaceLogin(t, bEmail, "ws-b", "rt-b0", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	bOutside := codexWorkspaceLogin(t, bEmail, "ws-b", "rt-b1", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	bRotated := codexWorkspaceLogin(t, bEmail, "ws-b", "rt-b2", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))

	_, store, credentials := openBackend(t, userHome, dbDir, "")
	if _, err := store.UpsertAndActivate(provideraccounts.Account{ID: "acct-a", Provider: codexName, Email: "same@example.com", OrgID: "ws-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(provideraccounts.Account{ID: "acct-b", Provider: codexName, Email: bEmail, OrgID: "ws-b"}); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential(codexName, "acct-a", aLogin); err != nil {
		t.Fatal(err)
	}
	if err := credentials.CommitSelectedCredentialForTest(codexName, "acct-a", aLogin, aLogin); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential(codexName, "acct-b", bSaved); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteNativeCredentialForTest(codexName, bOutside); err != nil {
		t.Fatal(err)
	}
	var probeResult []byte
	if rotated {
		probeResult = bRotated
	}
	homePath := crashSelectedUsageRefresh(t, userHome, dbDir, "acct-a", probeResult)
	asPreviousReleaseEntries(t, userHome)

	if otherRootFirst {
		otherDB := t.TempDir()
		other, _, _ := openBackend(t, userHome, otherDB, "")
		if err := bootProviderAccounts(other, userHome); err != nil {
			t.Fatal(err)
		}
		if got := mustReadCredential(t, credentials, "acct-a", false); got != string(aLogin) {
			t.Fatal("another data root's sweep put another login in A's slot")
		}
		if got := mustReadCredential(t, credentials, "", true); got != string(bOutside) {
			t.Fatal("another data root's sweep wrote the canonical credential")
		}
		_, statErr := os.Stat(homePath)
		if rotated != (statErr == nil) {
			t.Fatalf("after another data root's sweep, crashed home stat = %v; want it kept only for a rotation", statErr)
		}
	}

	manager, _, credentials := openBackend(t, userHome, dbDir, "")
	if err := bootProviderAccounts(manager, userHome); err != nil {
		t.Fatal(err)
	}
	if got := mustReadCredential(t, credentials, "acct-a", false); got != string(aLogin) {
		t.Fatal("A's slot no longer holds A's login after restart")
	}
	if got := mustReadCredential(t, credentials, "acct-b", false); got != string(bSaved) {
		t.Fatal("B's slot changed without B's login being identified")
	}
	wantCanonical := bOutside
	if rotated {
		wantCanonical = bRotated
	}
	if got := mustReadCredential(t, credentials, "", true); got != string(wantCanonical) {
		t.Fatal("canonical credential is not B's login at its latest rotation")
	}
	if _, err := os.Stat(homePath); !os.IsNotExist(err) {
		t.Fatalf("crashed probe home survived the owner's boot: %v", err)
	}
}

// codexIdentityProbe is a mock `codex app-server` that answers the
// identity probe's account/read with email's ChatGPT login.
func codexIdentityProbe(t *testing.T, email string) string {
	t.Helper()
	dir := t.TempDir()
	frame := filepath.Join(dir, "account-read.json")
	writeAccountReadFrame(t, frame, chatgptAccountFrame(email, "team", ""))
	return mockexec.Write(t, filepath.Join(dir, "mock-codex"), "#!/bin/bash\n"+
		"read -r _ || true\nread -r _ || true\nread -r _ || true\n"+
		"cat "+shellQuoteForTest(frame)+"\n")
}

// The selected account's slot lags its canonical credential (the CLI
// rotated the canonical copy in place), and its usage probe crashes after
// rotating that canonical credential in its temporary home. The boot sweep
// of the store that selected it restores the rotation to the canonical
// credential, and to the lagging slot through the token the slot held when
// the probe started, whichever data root sweeps first. A sign-in to another
// login before the owner boots leaves the slot to take it alone.
func TestBootBringsALaggingSelectedSlotToTheRecoveredRotation(t *testing.T) {
	for _, tc := range []struct {
		name           string
		otherRootFirst bool
		signInFirst    bool
	}{
		{name: "owner first"},
		{name: "other root first", otherRootFirst: true},
		{name: "sign-in before the owner boots", signInFirst: true},
		{name: "other root first, then a sign-in", otherRootFirst: true, signInFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kerneltest.IsolateSpawns(t)
			codexName := string(provider.Codex)
			userHome, dbDir := t.TempDir(), t.TempDir()
			slotLogin := codexWorkspaceLogin(t, "a@example.com", "ws-a", "rt-a1", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
			cliRotated := codexWorkspaceLogin(t, "a@example.com", "ws-a", "rt-a2", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
			probeRotated := codexWorkspaceLogin(t, "a@example.com", "ws-a", "rt-a3", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
			cLogin := codexWorkspaceLogin(t, "c@example.com", "ws-c", "rt-c1", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))

			_, store, credentials := openBackend(t, userHome, dbDir, "")
			if _, err := store.UpsertAndActivate(provideraccounts.Account{ID: "acct-a", Provider: codexName, Email: "a@example.com", OrgID: "ws-a"}); err != nil {
				t.Fatal(err)
			}
			if err := credentials.WriteAccountCredential(codexName, "acct-a", slotLogin); err != nil {
				t.Fatal(err)
			}
			if err := credentials.CommitSelectedCredentialForTest(codexName, "acct-a", slotLogin, slotLogin); err != nil {
				t.Fatal(err)
			}
			if err := credentials.WriteNativeCredentialForTest(codexName, cliRotated); err != nil {
				t.Fatal(err)
			}
			crashSelectedUsageRefresh(t, userHome, dbDir, "acct-a", probeRotated)

			if tc.otherRootFirst {
				otherDB := t.TempDir()
				other, _, _ := openBackend(t, userHome, otherDB, "")
				if err := bootProviderAccounts(other, userHome); err != nil {
					t.Fatal(err)
				}
				if got := mustReadCredential(t, credentials, "", true); got != string(cliRotated) {
					t.Fatal("another data root's sweep wrote the canonical credential")
				}
			}
			wantCanonical := probeRotated
			if tc.signInFirst {
				if err := credentials.WriteNativeCredentialForTest(codexName, cLogin); err != nil {
					t.Fatal(err)
				}
				wantCanonical = cLogin
			}

			manager, store, credentials := openBackend(t, userHome, dbDir, codexIdentityProbe(t, "a@example.com"))
			if err := bootProviderAccounts(manager, userHome); err != nil {
				t.Fatal(err)
			}
			if got := mustReadCredential(t, credentials, "", true); got != string(wantCanonical) {
				t.Fatal("the boot sweep left the canonical credential on the wrong login")
			}
			if got := mustReadCredential(t, credentials, "acct-a", false); got != string(probeRotated) {
				t.Fatal("the lagging slot did not take the recovered rotation")
			}
			if tc.signInFirst {
				return
			}
			if err := manager.ReconcileExternalProviderAccount(codexName); err != nil {
				t.Fatal(err)
			}
			if active, ok := store.Active(codexName, time.Now()); !ok || active.ID != "acct-a" {
				t.Fatalf("selection after reconciliation = %+v, %v; want acct-a", active, ok)
			}
		})
	}
}

// bootProviderAccounts runs the boot's account steps in the boot's order:
// the reserved slots of unfinished sign-ins, then the provider home.
func bootProviderAccounts(m *Manager, userHome string) error {
	return errors.Join(m.SettleReservedSlots(), m.ReconcileProviderHome(userHome))
}
