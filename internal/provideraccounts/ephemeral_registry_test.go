package provideraccounts

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/atomicfile"
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

// backdateRegistryEntry rewrites one home's registry entry so the sweep
// sees it as older than the grace period.
func backdateRegistryEntry(t *testing.T, c *Credentials, configHome string) {
	t.Helper()
	dir, err := c.ephemeralRegistryDir("claude")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ephemeralRegistryFileName(configHome))
	var entry ephemeralRegistryEntry
	found, err := atomicfile.ReadJSON(path, &entry)
	if err != nil || !found {
		t.Fatalf("registry entry for %s: found=%v err=%v", configHome, found, err)
	}
	entry.CreatedAt = entry.CreatedAt.Add(-2 * ephemeralSweepGrace)
	if err := atomicfile.WriteJSON(path, entry); err != nil {
		t.Fatal(err)
	}
}

// crashEphemeralClaudeHome creates a seeded ephemeral home and abandons
// it without Cleanup — the crash scenario the registry exists for —
// returning its path. The registry entry is backdated past the grace
// period so a sweep will act on it.
func crashEphemeralClaudeHome(t *testing.T, c *Credentials, credential []byte, ownerAccountID string) string {
	t.Helper()
	home, err := c.NewEphemeralHomeWithCredential("claude", credential, ownerAccountID)
	if err != nil {
		t.Fatal(err)
	}
	// The temp home lives outside t.TempDir; make sure an assertion
	// failure cannot leak it.
	t.Cleanup(func() { _ = os.RemoveAll(home.Path) })
	backdateRegistryEntry(t, c, home.Path)
	return home.Path
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
// provider's registry before any credential can exist in it and
// unrecorded only by a fully successful cleanup.
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
		if _, err := os.Stat(entryPath); err != nil {
			t.Fatalf("%s registry entry missing while home lives: %v", providerName, err)
		}
		if err := home.Cleanup(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(entryPath); !os.IsNotExist(err) {
			t.Fatalf("%s registry entry survived cleanup: %v", providerName, err)
		}
	}
}

// chainFixture orders "pos:N" credentials by N, the way the production
// policy orders Claude by token expiry and Codex by last_refresh.
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
	})
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

// crashEphemeralHome abandons a seeded home of any provider without
// Cleanup and backdates its registry entry past the grace period.
func crashEphemeralHome(t *testing.T, c *Credentials, providerName string, credential []byte, ownerAccountID string) string {
	t.Helper()
	home, err := c.NewEphemeralHomeWithCredential(providerName, credential, ownerAccountID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home.Path) })
	dir, err := c.ephemeralRegistryDir(providerName)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ephemeralRegistryFileName(home.Path))
	var entry ephemeralRegistryEntry
	if found, err := atomicfile.ReadJSON(path, &entry); err != nil || !found {
		t.Fatalf("%s registry entry: found=%v err=%v", providerName, found, err)
	}
	entry.CreatedAt = entry.CreatedAt.Add(-2 * ephemeralSweepGrace)
	if err := atomicfile.WriteJSON(path, entry); err != nil {
		t.Fatal(err)
	}
	return home.Path
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
			accountDir, err := credentials.accountDirectory("codex", "acct-1")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(accountDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := credentials.WriteAccountCredential("codex", "acct-1", []byte(tc.slot)); err != nil {
				t.Fatal(err)
			}
			homePath := crashEphemeralHome(t, credentials, "codex", []byte(tc.orphan), "acct-1")

			results, err := credentials.SweepEphemeralCredentials(time.Now())
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

// A crashed Codex login home owns no account. Its credential is never
// adopted anywhere, but the home and its live credential are removed.
func TestSweepRemovesACrashedCodexLoginHome(t *testing.T) {
	credentials := chainFixture(t)
	home, err := credentials.NewEphemeralHome("codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home.Path) })
	if err := os.WriteFile(filepath.Join(home.Path, "auth.json"), []byte("pos:5"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := credentials.ephemeralRegistryDir("codex")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ephemeralRegistryFileName(home.Path))
	var entry ephemeralRegistryEntry
	if found, err := atomicfile.ReadJSON(path, &entry); err != nil || !found {
		t.Fatalf("login home not registered: found=%v err=%v", found, err)
	}
	entry.CreatedAt = entry.CreatedAt.Add(-2 * ephemeralSweepGrace)
	if err := atomicfile.WriteJSON(path, entry); err != nil {
		t.Fatal(err)
	}

	results, err := credentials.SweepEphemeralCredentials(time.Now())
	if err != nil || len(results) != 1 || results[0].Action != "discarded" {
		t.Fatalf("sweep = %+v, %v; want the login home discarded", results, err)
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

	results, err := credentials.SweepEphemeralCredentials(time.Now())
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

	// The sweep is idempotent: nothing left to do on the next boot.
	results, err = credentials.SweepEphemeralCredentials(time.Now())
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

	results, err := credentials.SweepEphemeralCredentials(time.Now())
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

	results, err := credentials.SweepEphemeralCredentials(time.Now())
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

// Entries younger than the grace period may belong to another live
// instance's in-flight probe — the sweep must not touch them.
func TestSweepSkipsEntriesInsideTheGracePeriod(t *testing.T) {
	credentials := sweepFixture(t)
	home, err := credentials.NewEphemeralHomeWithCredential("claude", []byte("in-flight"), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = home.Cleanup() }()

	results, err := credentials.SweepEphemeralCredentials(time.Now())
	if err != nil || len(results) != 1 || results[0].Action != "skipped" {
		t.Fatalf("sweep = %+v, %v; want one skip", results, err)
	}
	if data, err := credentials.ReadEphemeralCredential(home); err != nil || string(data.Bytes()) != "in-flight" {
		t.Fatalf("in-flight home disturbed by sweep: %q, %v", data.Bytes(), err)
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

	results, err := credentials.SweepEphemeralCredentials(time.Now())
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
		CreatedAt:  time.Now().Add(-2 * ephemeralSweepGrace),
	}
	entryPath := filepath.Join(dir, ephemeralRegistryFileName(realDir))
	if err := atomicfile.WriteJSON(entryPath, entry); err != nil {
		t.Fatal(err)
	}

	results, err := credentials.SweepEphemeralCredentials(time.Now())
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

	results, err := credentials.SweepEphemeralCredentials(time.Now())
	if err != nil || len(results) != 1 || results[0].Action != "discarded" {
		t.Fatalf("sweep = %+v, %v; want discard for the deleted account", results, err)
	}
	if _, err := credentials.ReadCredential("claude", "deleted-acct", false); !IsCredentialMissing(err) {
		t.Fatalf("sweep resurrected a deleted account's slot: %v", err)
	}
}
