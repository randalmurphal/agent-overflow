package provideraccounts

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/atomicfile"
)

func sweepFixture(t *testing.T) *Credentials {
	t.Helper()
	credentials, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

// backdateRegistryEntry rewrites one home's registry entry so the sweep
// sees it as older than the grace period.
func backdateRegistryEntry(t *testing.T, c *Credentials, configHome string) {
	t.Helper()
	dir, err := c.ephemeralRegistryDir()
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

// Every ephemeral Claude home is recorded before any credential can
// exist in it and unrecorded only by a fully successful cleanup; codex
// homes stay out of the registry entirely.
func TestEphemeralClaudeHomesAreRegisteredForTheirLifetime(t *testing.T) {
	credentials := sweepFixture(t)
	dir, err := credentials.ephemeralRegistryDir()
	if err != nil {
		t.Fatal(err)
	}

	home, err := credentials.NewEphemeralHomeWithCredential("claude", []byte("seed"), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	entryPath := filepath.Join(dir, ephemeralRegistryFileName(home.Path))
	if _, err := os.Stat(entryPath); err != nil {
		t.Fatalf("registry entry missing while home lives: %v", err)
	}
	if err := home.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entryPath); !os.IsNotExist(err) {
		t.Fatalf("registry entry survived cleanup: %v", err)
	}

	codexHome, err := credentials.NewEphemeralHomeWithCredential("codex", []byte("seed"), "acct-2")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = codexHome.Cleanup() }()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("codex home registered %d entries, want 0", len(entries))
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

	results, err := credentials.SweepEphemeralClaudeCredentials(time.Now())
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
	results, err = credentials.SweepEphemeralClaudeCredentials(time.Now())
	if err != nil || len(results) != 0 {
		t.Fatalf("second sweep = %+v, %v; want empty", results, err)
	}
}

// A husk in the slot is as dead as an empty slot — the orphan's live
// bytes must still win.
func TestSweepAdoptsOverASignedOutHusk(t *testing.T) {
	credentials := sweepFixture(t)
	credentials.SetSignedOutDetector(func(providerName string, data []byte) bool {
		return string(data) == "husk"
	})
	makeSlotDir(t, credentials, "acct-1")
	if err := credentials.WriteAccountCredential("claude", "acct-1", []byte("husk")); err != nil {
		t.Fatal(err)
	}
	crashEphemeralClaudeHome(t, credentials, []byte("rotated-chain"), "acct-1")

	results, err := credentials.SweepEphemeralClaudeCredentials(time.Now())
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
	credentials.SetSignedOutDetector(func(providerName string, data []byte) bool {
		return string(data) == "husk"
	})

	makeSlotDir(t, credentials, "healthy")
	if err := credentials.WriteAccountCredential("claude", "healthy", []byte("fresh-relogin")); err != nil {
		t.Fatal(err)
	}
	crashEphemeralClaudeHome(t, credentials, []byte("stale-orphan"), "healthy")

	makeSlotDir(t, credentials, "empty")
	crashEphemeralClaudeHome(t, credentials, []byte("husk"), "empty")

	results, err := credentials.SweepEphemeralClaudeCredentials(time.Now())
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

	results, err := credentials.SweepEphemeralClaudeCredentials(time.Now())
	if err != nil || len(results) != 1 || results[0].Action != "skipped" {
		t.Fatalf("sweep = %+v, %v; want one skip", results, err)
	}
	if data, err := credentials.ReadEphemeralCredential(home); err != nil || string(data.Data) != "in-flight" {
		t.Fatalf("in-flight home disturbed by sweep: %q, %v", data.Data, err)
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

	results, err := credentials.SweepEphemeralClaudeCredentials(time.Now())
	if err != nil || len(results) != 1 || results[0].Action != "cleaned" {
		t.Fatalf("sweep = %+v, %v; want one cleaned entry", results, err)
	}
}

// The sweep acts only on paths it can prove are ephemeral homes: a
// registry entry pointing anywhere else — corrupt state, a foreign
// write — is dropped without touching the target.
func TestSweepRefusesToTouchNonEphemeralPaths(t *testing.T) {
	credentials := sweepFixture(t)
	dir, err := credentials.ephemeralRegistryDir()
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

	results, err := credentials.SweepEphemeralClaudeCredentials(time.Now())
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

	results, err := credentials.SweepEphemeralClaudeCredentials(time.Now())
	if err != nil || len(results) != 1 || results[0].Action != "discarded" {
		t.Fatalf("sweep = %+v, %v; want discard for the deleted account", results, err)
	}
	if _, err := credentials.ReadCredential("claude", "deleted-acct", false); !IsCredentialMissing(err) {
		t.Fatalf("sweep resurrected a deleted account's slot: %v", err)
	}
}
