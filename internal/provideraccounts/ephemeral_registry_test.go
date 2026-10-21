package provideraccounts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/filelock"
)

func sweepFixture(t *testing.T) *Credentials {
	t.Helper()
	// The sweep's husk rules ("never adopt a husk orphan", "adopt over a
	// husked slot") consult the detector, so the fixture carries it the way
	// production does.
	credentials, err := NewCredentials(t.TempDir(), Policy{SignedOut: literalHuskDetector})
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

// abandonEphemeralHome ends a home's lease without cleaning it up: what
// the kernel does when the owning process dies.
func abandonEphemeralHome(t *testing.T, home *EphemeralHome) {
	t.Helper()
	if err := filelock.Release(home.lease); err != nil {
		t.Fatal(err)
	}
	home.lease = nil
	// The temp home lives outside t.TempDir; make sure an assertion
	// failure cannot leak it.
	t.Cleanup(func() { _ = os.RemoveAll(home.Path) })
}

// crashEphemeralHome creates a seeded ephemeral home and abandons it
// without Cleanup, the crash the registry exists for, returning its path.
// The entry is as young as a crash right before a restart leaves it.
func crashEphemeralHome(t *testing.T, c *Credentials, providerName string, credential []byte, ownerAccountID string) string {
	t.Helper()
	home, err := c.NewEphemeralHomeWithCredential(providerName, credential, ownerAccountID)
	if err != nil {
		t.Fatal(err)
	}
	abandonEphemeralHome(t, home)
	return home.Path
}

func crashEphemeralClaudeHome(t *testing.T, c *Credentials, credential []byte, ownerAccountID string) string {
	t.Helper()
	return crashEphemeralHome(t, c, "claude", credential, ownerAccountID)
}

// writeProviderAuthoredCredential puts bytes into a store the way the CLI
// does, bypassing the sign-out refusal Agent Overflow's own writes carry. It
// is how a fixture reproduces a husk, which only the provider ever authors.
func writeProviderAuthoredCredential(t *testing.T, home string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, claudeCredentialFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeSlotDir(t *testing.T, c *Credentials, accountID string) string {
	t.Helper()
	accountDir, err := c.accountDirectory("claude", accountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return accountDir
}

// Every ephemeral home, whatever its provider, is recorded in that
// provider's registry with a held lease before any credential can exist
// in it, and unrecorded only by a fully successful cleanup.
func TestEphemeralHomesAreRegisteredForTheirLifetime(t *testing.T) {
	credentials := sweepFixture(t)
	for _, providerName := range ephemeralProviders {
		dir, err := credentials.ephemeralRegistryDir(providerName)
		if err != nil {
			t.Fatal(err)
		}
		home, err := credentials.NewEphemeralHomeWithCredential(providerName, []byte("seed"), "acct-1")
		if err != nil {
			t.Fatal(err)
		}
		entryPath := filepath.Join(dir, ephemeralRegistryFileName(home.Path))
		leasePath := filepath.Join(dir, ephemeralLeaseFileName(home.Path))
		if _, err := os.Stat(entryPath); err != nil {
			t.Fatalf("%s registry entry missing while home lives: %v", providerName, err)
		}
		probe, err := filelock.Open(leasePath)
		if err != nil {
			t.Fatal(err)
		}
		if locked, err := filelock.TryLock(probe); locked || err != nil {
			t.Fatalf("%s lease not held while home lives: locked=%v err=%v", providerName, locked, err)
		}
		if err := probe.Close(); err != nil {
			t.Fatal(err)
		}
		if err := home.Cleanup(); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{entryPath, leasePath} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("%s registry file %s survived cleanup: %v", providerName, filepath.Base(path), err)
			}
		}
	}
}

// chainFixture orders "pos:N" credentials by N, the way the production
// policy orders Claude by token expiry and Codex by last_refresh, and names
// the login of "pos:N login:X" credentials X.
func chainFixture(t *testing.T) *Credentials {
	t.Helper()
	credentials, err := NewCredentials(t.TempDir(), Policy{
		SignedOut: literalHuskDetector,
		ChainPosition: func(_ string, data []byte) (int64, bool) {
			var position int64
			if _, err := fmt.Sscanf(string(data), "pos:%d", &position); err != nil {
				return 0, false
			}
			return position, true
		},
		LoginID: func(_ string, data []byte) (string, bool) {
			_, login, ok := strings.Cut(string(data), " login:")
			return login, ok && login != ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

func writeCodexSlot(t *testing.T, c *Credentials, accountID, credential string) {
	t.Helper()
	accountDir, err := c.accountDirectory("codex", accountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteAccountCredential("codex", accountID, []byte(credential)); err != nil {
		t.Fatal(err)
	}
}

// A Codex usage probe rotates the chain inside its temporary home and the
// caller copies the rotation into the slot afterwards. A crash in between
// leaves the slot healthy-looking but behind: the sweep restores the
// orphan when it is strictly later on the chain, never otherwise, and
// always removes the abandoned home.
func TestSweepRecoversACrashedCodexRotation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		slot   string
		orphan string
		action string
		want   string
	}{
		{name: "orphan ahead of the slot", slot: "pos:10", orphan: "pos:20", action: "adopted", want: "pos:20"},
		{name: "slot ahead of the orphan", slot: "pos:30", orphan: "pos:20", action: "discarded", want: "pos:30"},
		{name: "same position", slot: "pos:20", orphan: "pos:20", action: "discarded", want: "pos:20"},
		{name: "slot position unknown", slot: "relogin", orphan: "pos:20", action: "discarded", want: "relogin"},
		{name: "orphan position unknown", slot: "pos:10", orphan: "api-key", action: "discarded", want: "pos:10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credentials := chainFixture(t)
			writeCodexSlot(t, credentials, "acct-1", tc.slot)
			homePath := crashEphemeralHome(t, credentials, "codex", []byte(tc.orphan), "acct-1")

			results, err := credentials.SweepEphemeralCredentials(time.Now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Provider != "codex" || results[0].Action != tc.action {
				t.Fatalf("sweep results = %+v, want one codex %s", results, tc.action)
			}
			slot, err := credentials.ReadCredential("codex", "acct-1", false)
			if err != nil || string(slot) != tc.want {
				t.Fatalf("slot = %q, %v; want %q", slot, err, tc.want)
			}
			if _, err := os.Stat(homePath); !os.IsNotExist(err) {
				t.Fatalf("crashed codex home survived the sweep: %v", err)
			}
		})
	}
}

// The selected account's probe starts from the canonical credential, so a
// crash after its rotation leaves the canonical store as far behind as the
// slot. The sweep brings both forward, but only over the same login: a
// different login in the canonical store is an outside sign-in.
func TestSweepRestoresTheSelectedAccountsCanonicalCredential(t *testing.T) {
	for _, tc := range []struct {
		name          string
		selected      map[string]string
		canonical     string
		wantCanonical string
		wantReplaced  bool
	}{
		{name: "same login behind", selected: map[string]string{"codex": "acct-1"}, canonical: "pos:10 login:alice", wantCanonical: "pos:20 login:alice", wantReplaced: true},
		{name: "outside sign-in", selected: map[string]string{"codex": "acct-1"}, canonical: "pos:5 login:bob", wantCanonical: "pos:5 login:bob"},
		{name: "same login ahead", selected: map[string]string{"codex": "acct-1"}, canonical: "pos:30 login:alice", wantCanonical: "pos:30 login:alice"},
		{name: "canonical login unknown", selected: map[string]string{"codex": "acct-1"}, canonical: "pos:10", wantCanonical: "pos:10"},
		{name: "account not selected", selected: map[string]string{"codex": "acct-2"}, canonical: "pos:10 login:alice", wantCanonical: "pos:10 login:alice"},
		{name: "no canonical credential", selected: map[string]string{"codex": "acct-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credentials := chainFixture(t)
			writeCodexSlot(t, credentials, "acct-1", "pos:10 login:alice")
			if tc.canonical != "" {
				if err := credentials.writeActiveCredential("codex", []byte(tc.canonical)); err != nil {
					t.Fatal(err)
				}
			}
			crashEphemeralHome(t, credentials, "codex", []byte("pos:20 login:alice"), "acct-1")

			results, err := credentials.SweepEphemeralCredentials(time.Now(), tc.selected)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Action != "adopted" || results[0].Canonical != tc.wantReplaced {
				t.Fatalf("sweep results = %+v, want adoption with canonical=%v", results, tc.wantReplaced)
			}
			slot, err := credentials.ReadCredential("codex", "acct-1", false)
			if err != nil || string(slot) != "pos:20 login:alice" {
				t.Fatalf("slot = %q, %v; want the orphan", slot, err)
			}
			canonical, err := credentials.ReadCredential("codex", "", true)
			if tc.wantCanonical == "" {
				if !IsCredentialMissing(err) {
					t.Fatalf("canonical = %q, %v; want none written", canonical, err)
				}
				return
			}
			if err != nil || string(canonical) != tc.wantCanonical {
				t.Fatalf("canonical = %q, %v; want %q", canonical, err, tc.wantCanonical)
			}
		})
	}
}

// Adoption decides and writes under the slot lock, so a slot write from
// another process lands wholly before the decision or after it. Here a
// newer credential lands while the sweep waits for the lock, and the sweep
// must see it and keep it.
func TestSweepAdoptionHoldsTheSlotLock(t *testing.T) {
	credentials := chainFixture(t)
	writeCodexSlot(t, credentials, "acct-1", "pos:10")
	crashEphemeralHome(t, credentials, "codex", []byte("pos:20"), "acct-1")
	accountDir, err := credentials.accountDirectory("codex", "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockSlot(accountDir)
	if err != nil {
		t.Fatal(err)
	}

	type sweep struct {
		results []EphemeralSweepResult
		err     error
	}
	done := make(chan sweep, 1)
	go func() {
		results, err := credentials.SweepEphemeralCredentials(time.Now(), nil)
		done <- sweep{results, err}
	}()
	select {
	case got := <-done:
		_ = releaseSlot(lock)
		t.Fatalf("sweep finished while another writer held the slot: %+v, %v", got.results, got.err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := credentials.storeCredentialAt("codex", accountDir, false, []byte("pos:30")); err != nil {
		t.Fatal(err)
	}
	if err := releaseSlot(lock); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || len(got.results) != 1 || got.results[0].Action != "discarded" {
		t.Fatalf("sweep = %+v, %v; want the orphan discarded behind the newer write", got.results, got.err)
	}
	slot, err := credentials.ReadCredential("codex", "acct-1", false)
	if err != nil || string(slot) != "pos:30" {
		t.Fatalf("slot = %q, %v; want the newer write kept", slot, err)
	}
}

// A crashed Codex login home owns no account. Its credential is never
// adopted anywhere, but the home and its live credential are removed.
func TestSweepRemovesACrashedCodexLoginHome(t *testing.T) {
	credentials := chainFixture(t)
	home, err := credentials.NewEphemeralHome("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.Path, "auth.json"), []byte("pos:5"), 0o600); err != nil {
		t.Fatal(err)
	}
	abandonEphemeralHome(t, home)

	results, err := credentials.SweepEphemeralCredentials(time.Now(), nil)
	if err != nil || len(results) != 1 || results[0].Action != "discarded" {
		t.Fatalf("sweep = %+v, %v; want the login home discarded", results, err)
	}
	if _, err := os.Stat(home.Path); !os.IsNotExist(err) {
		t.Fatalf("crashed login home survived: %v", err)
	}
}

// A login home whose owner died before any sign-in landed holds nothing;
// the sweep removes it at once.
func TestSweepCleansAnUnseededLoginHome(t *testing.T) {
	credentials := chainFixture(t)
	home, err := credentials.NewEphemeralHome("codex")
	if err != nil {
		t.Fatal(err)
	}
	abandonEphemeralHome(t, home)

	results, err := credentials.SweepEphemeralCredentials(time.Now(), nil)
	if err != nil || len(results) != 1 || results[0].Action != "cleaned" {
		t.Fatalf("sweep = %+v, %v; want the empty login home cleaned", results, err)
	}
	if _, err := os.Stat(home.Path); !os.IsNotExist(err) {
		t.Fatalf("crashed login home survived: %v", err)
	}
}

// The core recovery: a crash after the CLI rotated the single-use chain
// leaves the only live copy in the abandoned home. When the owning slot
// is dead (missing or husk), the sweep restores those bytes before
// deleting the remnants.
func TestSweepAdoptsOrphanIntoADeadSlot(t *testing.T) {
	credentials := sweepFixture(t)
	makeSlotDir(t, credentials, "acct-1")
	homePath := crashEphemeralClaudeHome(t, credentials, []byte("rotated-chain"), "acct-1")

	results, err := credentials.SweepEphemeralCredentials(time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Action != "adopted" || results[0].AccountID != "acct-1" {
		t.Fatalf("sweep results = %+v, want one adoption for acct-1", results)
	}
	slot, err := credentials.ReadCredential("claude", "acct-1", false)
	if err != nil || string(slot) != "rotated-chain" {
		t.Fatalf("slot after adoption = %q, %v; want the orphan bytes", slot, err)
	}
	if _, err := os.Stat(homePath); !os.IsNotExist(err) {
		t.Fatalf("crashed home survived the sweep: %v", err)
	}
	dir, err := credentials.ephemeralRegistryDir("claude")
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("registry after the sweep = %v, %v; want empty", entries, err)
	}

	// The sweep is idempotent: nothing left to do on the next boot.
	results, err = credentials.SweepEphemeralCredentials(time.Now(), nil)
	if err != nil || len(results) != 0 {
		t.Fatalf("second sweep = %+v, %v; want empty", results, err)
	}
}

// A husk in the slot is as dead as an empty slot — the orphan's live
// bytes must still win.
func TestSweepAdoptsOverASignedOutHusk(t *testing.T) {
	credentials := sweepFixture(t)
	writeProviderAuthoredCredential(t, makeSlotDir(t, credentials, "acct-1"), []byte("husk"))
	crashEphemeralClaudeHome(t, credentials, []byte("rotated-chain"), "acct-1")

	results, err := credentials.SweepEphemeralCredentials(time.Now(), nil)
	if err != nil || len(results) != 1 || results[0].Action != "adopted" {
		t.Fatalf("sweep = %+v, %v; want adoption over the husk", results, err)
	}
	slot, err := credentials.ReadCredential("claude", "acct-1", false)
	if err != nil || string(slot) != "rotated-chain" {
		t.Fatalf("slot = %q, %v; want the orphan bytes", slot, err)
	}
}

// A healthy slot is NEVER overwritten: the user may have re-logged-in
// since the crash, and those bytes are newer than any orphan. The
// orphan itself, when it is a husk, is never adopted anywhere.
func TestSweepNeverOverwritesAHealthySlotAndNeverAdoptsAHusk(t *testing.T) {
	credentials := sweepFixture(t)

	makeSlotDir(t, credentials, "healthy")
	if err := credentials.WriteAccountCredential("claude", "healthy", []byte("fresh-relogin")); err != nil {
		t.Fatal(err)
	}
	crashEphemeralClaudeHome(t, credentials, []byte("stale-orphan"), "healthy")

	makeSlotDir(t, credentials, "empty")
	// The husk arrives the way it does in life: the CLI blanks the credential
	// inside the temporary home when its refresh comes back invalid_grant.
	huskHome := crashEphemeralClaudeHome(t, credentials, []byte("live-seed"), "empty")
	writeProviderAuthoredCredential(t, huskHome, []byte("husk"))

	results, err := credentials.SweepEphemeralCredentials(time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("sweep results = %+v, want 2 discards", results)
	}
	for _, result := range results {
		if result.Action != "discarded" {
			t.Fatalf("result %+v, want discarded", result)
		}
	}
	slot, err := credentials.ReadCredential("claude", "healthy", false)
	if err != nil || string(slot) != "fresh-relogin" {
		t.Fatalf("healthy slot = %q, %v; must be untouched", slot, err)
	}
	if _, err := credentials.ReadCredential("claude", "empty", false); !IsCredentialMissing(err) {
		t.Fatalf("husk orphan reached the empty slot: %v", err)
	}
}

// A home whose owner still runs is never touched, however old its entry:
// a stopped or suspended process can hold a home far longer than any probe
// takes. Once the owner is gone the next sweep recovers it.
func TestSweepSkipsHomesWhoseOwnerIsRunning(t *testing.T) {
	credentials := sweepFixture(t)
	makeSlotDir(t, credentials, "acct-1")
	home, err := credentials.NewEphemeralHomeWithCredential("claude", []byte("in-flight"), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = home.Cleanup() }()

	results, err := credentials.SweepEphemeralCredentials(time.Now().Add(30*24*time.Hour), nil)
	if err != nil || len(results) != 1 || results[0].Action != "skipped" {
		t.Fatalf("sweep = %+v, %v; want one skip", results, err)
	}
	if data, err := credentials.ReadEphemeralCredential(home); err != nil || string(data.Bytes()) != "in-flight" {
		t.Fatalf("in-flight home disturbed by sweep: %q, %v", data.Bytes(), err)
	}

	abandonEphemeralHome(t, home)
	results, err = credentials.SweepEphemeralCredentials(time.Now(), nil)
	if err != nil || len(results) != 1 || results[0].Action != "adopted" {
		t.Fatalf("sweep after the owner died = %+v, %v; want adoption", results, err)
	}
}

// Entries without a lease were written by a build that predates leases,
// which may still be running: they are judged by age.
func TestSweepJudgesUnleasedEntriesByAge(t *testing.T) {
	credentials := sweepFixture(t)
	home, err := credentials.NewEphemeralHomeWithCredential("claude", []byte("in-flight"), "")
	if err != nil {
		t.Fatal(err)
	}
	abandonEphemeralHome(t, home)
	dir, err := credentials.ephemeralRegistryDir("claude")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, ephemeralLeaseFileName(home.Path))); err != nil {
		t.Fatal(err)
	}

	results, err := credentials.SweepEphemeralCredentials(time.Now(), nil)
	if err != nil || len(results) != 1 || results[0].Action != "skipped" {
		t.Fatalf("sweep of a young unleased entry = %+v, %v; want one skip", results, err)
	}
	results, err = credentials.SweepEphemeralCredentials(time.Now().Add(2*unleasedEntryGrace), nil)
	if err != nil || len(results) != 1 || results[0].Action != "discarded" {
		t.Fatalf("sweep of an old unleased entry = %+v, %v; want it discarded", results, err)
	}
	if _, err := os.Stat(home.Path); !os.IsNotExist(err) {
		t.Fatalf("old unleased home survived: %v", err)
	}
}

// A sweep holds the lease of the entry it is working on, so a second
// sweep sees that entry as owned.
func TestTwoSweepsNeverClaimOneEntry(t *testing.T) {
	credentials := sweepFixture(t)
	homePath := crashEphemeralClaudeHome(t, credentials, []byte("rotated-chain"), "")
	dir, err := credentials.ephemeralRegistryDir("claude")
	if err != nil {
		t.Fatal(err)
	}
	entryPath := filepath.Join(dir, ephemeralRegistryFileName(homePath))
	leasePath := filepath.Join(dir, ephemeralLeaseFileName(homePath))
	entry := ephemeralRegistryEntry{ConfigHome: homePath, CreatedAt: time.Now()}
	lease, claim, err := claimEphemeralEntry(entryPath, leasePath, entry, time.Now())
	if err != nil || claim != entryClaimed {
		t.Fatalf("first claim = %v, %v", claim, err)
	}
	defer func() { _ = filelock.Release(lease) }()
	if _, claim, err := claimEphemeralEntry(entryPath, leasePath, entry, time.Now()); err != nil || claim != entryOwned {
		t.Fatalf("second claim = %v, %v; want owned", claim, err)
	}
}

// A crash can also strike after the credential was already removed; the
// sweep then just clears the bookkeeping.
func TestSweepCleansEntriesWithNothingLeftBehind(t *testing.T) {
	credentials := sweepFixture(t)
	homePath := crashEphemeralClaudeHome(t, credentials, []byte("gone"), "acct-1")
	if err := os.RemoveAll(homePath); err != nil {
		t.Fatal(err)
	}

	results, err := credentials.SweepEphemeralCredentials(time.Now(), nil)
	if err != nil || len(results) != 1 || results[0].Action != "cleaned" {
		t.Fatalf("sweep = %+v, %v; want one cleaned entry", results, err)
	}
}

// The sweep acts only on paths it can prove are ephemeral homes: a
// registry entry pointing anywhere else — corrupt state, a foreign
// write — is dropped without touching the target.
func TestSweepRefusesToTouchNonEphemeralPaths(t *testing.T) {
	credentials := sweepFixture(t)
	dir, err := credentials.ephemeralRegistryDir("claude")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	realDir := t.TempDir()
	realFile := filepath.Join(realDir, claudeCredentialFileName)
	if err := os.WriteFile(realFile, []byte("real-login"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := ephemeralRegistryEntry{
		ConfigHome: realDir,
		AccountID:  "acct-1",
		CreatedAt:  time.Now().Add(-2 * unleasedEntryGrace),
	}
	entryPath := filepath.Join(dir, ephemeralRegistryFileName(realDir))
	if err := atomicfile.WriteJSON(entryPath, entry); err != nil {
		t.Fatal(err)
	}

	results, err := credentials.SweepEphemeralCredentials(time.Now(), nil)
	if err != nil || len(results) != 0 {
		t.Fatalf("sweep = %+v, %v; want the unsafe entry silently dropped", results, err)
	}
	if data, readErr := os.ReadFile(realFile); readErr != nil || string(data) != "real-login" {
		t.Fatalf("sweep touched a non-ephemeral path: %q, %v", data, readErr)
	}
	if _, err := os.Stat(entryPath); !os.IsNotExist(err) {
		t.Fatal("unsafe registry entry must be dropped")
	}
}

// Adoption requires the slot directory to still exist: the prune runs
// first, so a missing directory means the account was deleted and the
// orphan has no home to return to.
func TestSweepDiscardsWhenTheAccountWasRemoved(t *testing.T) {
	credentials := sweepFixture(t)
	crashEphemeralClaudeHome(t, credentials, []byte("rotated-chain"), "deleted-acct")

	results, err := credentials.SweepEphemeralCredentials(time.Now(), nil)
	if err != nil || len(results) != 1 || results[0].Action != "discarded" {
		t.Fatalf("sweep = %+v, %v; want discard for the deleted account", results, err)
	}
	if _, err := credentials.ReadCredential("claude", "deleted-acct", false); !IsCredentialMissing(err) {
		t.Fatalf("sweep resurrected a deleted account's slot: %v", err)
	}
}
