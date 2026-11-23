package provideraccountapp

import (
	"encoding/base64"
	"encoding/json"
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
func TestCrashedSelectedUsageRefreshHelper(t *testing.T) {
	userHome := os.Getenv(crashedRefreshUserHomeEnv)
	if userHome == "" {
		t.Skip("helper process only")
	}
	manager, _, _ := openBackend(t, userHome, os.Getenv(crashedRefreshDBDirEnv), os.Getenv(crashedRefreshBinaryEnv))
	err := manager.RefreshProviderAccountUsage(string(provider.Codex), os.Getenv(crashedRefreshAccountEnv))
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
	if err := credentials.CommitSelectedCredential(codexName, "acct-a", aLogin, aLogin); err != nil {
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

	if otherRootFirst {
		otherDB := t.TempDir()
		other, _, _ := openBackend(t, userHome, otherDB, "")
		if err := other.ReconcileProviderHome(otherDB, userHome); err != nil {
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
	if err := manager.ReconcileProviderHome(dbDir, userHome); err != nil {
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

// The Codex CLI rotated the selected account's login in place, so the
// canonical credential is ahead of the slot, and a usage refresh died after
// its probe rotated the canonical credential again in its temporary home.
// The sweep cannot prove the rotation is the slot's login and leaves the
// slot alone; it restores the canonical credential, and the reconciliation
// that follows identifies it and brings the slot to it. When another data
// root sweeps first it keeps the entry, and the owner's boot does the same.
func TestBootBringsALaggingSelectedSlotToTheRecoveredRotation(t *testing.T) {
	for _, otherRootFirst := range []bool{false, true} {
		t.Run("other_root_first="+strconv.FormatBool(otherRootFirst), func(t *testing.T) {
			kerneltest.IsolateSpawns(t)
			codexName := string(provider.Codex)
			userHome, dbDir := t.TempDir(), t.TempDir()
			slotLogin := codexWorkspaceLogin(t, "a@example.com", "ws-a", "rt-a1", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
			cliRotated := codexWorkspaceLogin(t, "a@example.com", "ws-a", "rt-a2", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
			probeRotated := codexWorkspaceLogin(t, "a@example.com", "ws-a", "rt-a3", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))

			_, store, credentials := openBackend(t, userHome, dbDir, "")
			if _, err := store.UpsertAndActivate(provideraccounts.Account{ID: "acct-a", Provider: codexName, Email: "a@example.com", OrgID: "ws-a"}); err != nil {
				t.Fatal(err)
			}
			if err := credentials.WriteAccountCredential(codexName, "acct-a", slotLogin); err != nil {
				t.Fatal(err)
			}
			if err := credentials.CommitSelectedCredential(codexName, "acct-a", slotLogin, slotLogin); err != nil {
				t.Fatal(err)
			}
			if err := credentials.WriteNativeCredentialForTest(codexName, cliRotated); err != nil {
				t.Fatal(err)
			}
			crashSelectedUsageRefresh(t, userHome, dbDir, "acct-a", probeRotated)

			if otherRootFirst {
				otherDB := t.TempDir()
				other, _, _ := openBackend(t, userHome, otherDB, "")
				if err := other.ReconcileProviderHome(otherDB, userHome); err != nil {
					t.Fatal(err)
				}
				if got := mustReadCredential(t, credentials, "", true); got != string(cliRotated) {
					t.Fatal("another data root's sweep wrote the canonical credential")
				}
			}

			manager, store, credentials := openBackend(t, userHome, dbDir, codexIdentityProbe(t, "a@example.com"))
			if err := manager.ReconcileProviderHome(dbDir, userHome); err != nil {
				t.Fatal(err)
			}
			if got := mustReadCredential(t, credentials, "", true); got != string(probeRotated) {
				t.Fatal("the boot sweep did not restore the rotation to the canonical credential")
			}
			if got := mustReadCredential(t, credentials, "acct-a", false); got != string(slotLogin) {
				t.Fatal("the boot sweep wrote a slot that never held the seed")
			}
			if err := manager.ReconcileExternalProviderAccount(codexName); err != nil {
				t.Fatal(err)
			}
			if got := mustReadCredential(t, credentials, "acct-a", false); got != string(probeRotated) {
				t.Fatal("reconciliation did not bring the slot to the recovered rotation")
			}
			if active, ok := store.Active(codexName, time.Now()); !ok || active.ID != "acct-a" {
				t.Fatalf("selection after reconciliation = %+v, %v; want acct-a", active, ok)
			}
		})
	}
}
