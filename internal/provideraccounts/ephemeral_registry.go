package provideraccounts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
)

// The ephemeral registry closes the crash window on temporary Claude
// homes. Every ephemeral Claude home gets a Keychain item (darwin) or a
// credential file seeded or written by the CLI, and a probe there can
// ROTATE the account's single-use refresh chain — so a crash between
// the rotation and the read-back leaves the only live copy of that
// chain in a store nothing references: the temp directory is gone, but
// the hash-named Keychain item is not. Each home is therefore recorded
// in a per-entry file BEFORE any credential can exist there, the entry
// is removed on clean teardown, and the boot sweep recovers whatever a
// crash left behind: adopt the bytes into the owning slot when the slot
// is dead, then delete the remnant.
//
// One file per entry (not one shared file) so two concurrent app
// instances over the same home can never lose each other's records to
// a read-modify-write race: entries are independent, creation is
// atomic, and double-removal is a no-op.
const ephemeralRegistryDirName = "agent-overflow-ephemerals"

// ephemeralSweepGrace keeps the sweep off entries another live instance
// may still be using: probes finish in under a minute and a login flow
// in minutes, so anything older than an hour belongs to a crashed run.
// A younger entry just waits for a later boot.
const ephemeralSweepGrace = time.Hour

// ephemeralClaudeHomePrefix is the os.MkdirTemp prefix newEphemeralHome
// uses for Claude homes. The sweep refuses to act on any recorded path
// without it — a corrupted or foreign registry entry must never aim the
// remnant removal at a real directory.
const ephemeralClaudeHomePrefix = "agent-overflow-claude-"

type ephemeralRegistryEntry struct {
	ConfigHome string    `json:"configHome"`
	AccountID  string    `json:"accountId,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// EphemeralSweepResult reports what the boot sweep did with one
// crash-orphaned ephemeral Claude home.
type EphemeralSweepResult struct {
	ConfigHome string
	AccountID  string
	// Action is one of "adopted" (the orphan held the only live copy of
	// the owning account's chain and was restored into its slot),
	// "discarded" (a credential was left behind but the slot is
	// healthy), "cleaned" (no credential remained), or "skipped" (entry
	// younger than the grace period — possibly another live instance's
	// probe).
	Action string
}

func (c *Credentials) ephemeralRegistryDir() (string, error) {
	paths, err := c.Paths("claude")
	if err != nil {
		return "", err
	}
	return filepath.Join(paths.SharedHome, ephemeralRegistryDirName), nil
}

func ephemeralRegistryFileName(configHome string) string {
	sum := sha256.Sum256([]byte(configHome))
	return hex.EncodeToString(sum[:8]) + ".json"
}

// registerEphemeralClaudeHome records a temporary Claude home before
// any credential can exist in it. Ordering is the point: an
// unrecorded home that crashes is invisible forever, so registration
// failure fails the home's creation rather than proceeding uncovered.
func (c *Credentials) registerEphemeralClaudeHome(configHome, ownerAccountID string) error {
	dir, err := c.ephemeralRegistryDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("provideraccounts: create ephemeral registry: %w", err)
	}
	entry := ephemeralRegistryEntry{
		ConfigHome: configHome,
		AccountID:  ownerAccountID,
		CreatedAt:  time.Now().UTC(),
	}
	if err := atomicfile.WriteJSON(filepath.Join(dir, ephemeralRegistryFileName(configHome)), entry); err != nil {
		return fmt.Errorf("provideraccounts: record ephemeral Claude home: %w", err)
	}
	return nil
}

func (c *Credentials) unregisterEphemeralClaudeHome(configHome string) error {
	dir, err := c.ephemeralRegistryDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ephemeralRegistryFileName(configHome))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("provideraccounts: clear ephemeral Claude home record: %w", err)
	}
	return nil
}

// sweepableEphemeralHome is the structural guard on recorded paths: the
// sweep only ever acts on absolute paths whose directory name carries
// the ephemeral MkdirTemp prefix, so no registry content — corrupt,
// stale, or hostile — can point the removal or the Keychain-service
// hash at the canonical home, a slot, or any other real location.
func sweepableEphemeralHome(configHome string) bool {
	return filepath.IsAbs(configHome) &&
		strings.HasPrefix(filepath.Base(configHome), ephemeralClaudeHomePrefix)
}

// SweepEphemeralClaudeCredentials recovers and removes credentials left
// behind by ephemeral Claude homes that never reached their cleanup —
// a crash or kill mid-probe. Run at boot, after the orphan-slot prune.
//
// Per entry older than the grace period: read whatever credential the
// home still holds (Keychain item on darwin, credential file
// elsewhere); if the owning account's slot still exists and holds no
// usable credential (missing or the provider's sign-out husk) while the
// orphan's bytes are usable, restore them into the slot — the orphan
// may be the only live copy of a rotated single-use chain. Then delete
// the remnant credential, the leftover directory, and the entry. Any
// step that cannot prove it is safe keeps the entry for the next boot
// instead of guessing.
func (c *Credentials) SweepEphemeralClaudeCredentials(now time.Time) ([]EphemeralSweepResult, error) {
	dir, err := c.ephemeralRegistryDir()
	if err != nil {
		return nil, err
	}
	dirEntries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("provideraccounts: read ephemeral registry: %w", err)
	}

	var results []EphemeralSweepResult
	var sweepErrs []error
	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() || !strings.HasSuffix(dirEntry.Name(), ".json") {
			continue
		}
		entryPath := filepath.Join(dir, dirEntry.Name())
		var entry ephemeralRegistryEntry
		found, readErr := atomicfile.ReadJSON(entryPath, &entry)
		if readErr != nil || !found || !sweepableEphemeralHome(entry.ConfigHome) {
			// Unreadable or unsafe entries can never become actionable;
			// keeping them would re-report forever.
			if readErr != nil {
				sweepErrs = append(sweepErrs, fmt.Errorf("provideraccounts: drop unreadable ephemeral registry entry %s: %w", dirEntry.Name(), readErr))
			}
			if removeErr := os.Remove(entryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				sweepErrs = append(sweepErrs, removeErr)
			}
			continue
		}
		if now.Sub(entry.CreatedAt) < ephemeralSweepGrace {
			results = append(results, EphemeralSweepResult{
				ConfigHome: entry.ConfigHome,
				AccountID:  entry.AccountID,
				Action:     "skipped",
			})
			continue
		}

		result, entryErr := c.sweepOneEphemeralHome(entry)
		if entryErr != nil {
			// Kept for the next boot: the orphan may still hold the only
			// live copy of a chain, so an indeterminate state is never
			// resolved by deleting.
			sweepErrs = append(sweepErrs, entryErr)
			continue
		}
		if removeErr := os.Remove(entryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			sweepErrs = append(sweepErrs, removeErr)
			continue
		}
		results = append(results, result)
	}
	return results, errors.Join(sweepErrs...)
}

func (c *Credentials) sweepOneEphemeralHome(entry ephemeralRegistryEntry) (EphemeralSweepResult, error) {
	result := EphemeralSweepResult{ConfigHome: entry.ConfigHome, AccountID: entry.AccountID}

	orphan, readErr := c.readCredentialAt("claude", entry.ConfigHome, false)
	if readErr != nil && !IsCredentialMissing(readErr) {
		return result, fmt.Errorf("provideraccounts: read crash-orphaned ephemeral credential at %s: %w", entry.ConfigHome, readErr)
	}
	if IsCredentialMissing(readErr) {
		result.Action = "cleaned"
		return result, c.removeEphemeralRemnants(entry.ConfigHome)
	}

	result.Action = "discarded"
	if entry.AccountID != "" && !c.credentialSignedOut("claude", orphan.Data) {
		adopt, decideErr := c.slotNeedsAdoption(entry.AccountID)
		if decideErr != nil {
			return result, decideErr
		}
		if adopt {
			if writeErr := c.WriteAccountCredential("claude", entry.AccountID, orphan.Data); writeErr != nil {
				return result, fmt.Errorf("provideraccounts: restore crash-orphaned credential to slot %s: %w", entry.AccountID, writeErr)
			}
			result.Action = "adopted"
		}
	}
	return result, c.removeEphemeralRemnants(entry.ConfigHome)
}

// slotNeedsAdoption reports whether the account's slot would gain from
// the orphan's bytes: the slot directory still exists (the account is
// still saved on this machine — the prune runs first) but its
// credential is missing or the provider's own sign-out husk. A healthy
// slot is never overwritten: the user may have re-logged-in since the
// crash, and those bytes are newer than any orphan.
func (c *Credentials) slotNeedsAdoption(accountID string) (bool, error) {
	accountDir, err := c.accountDirectory("claude", accountID)
	if err != nil {
		return false, nil
	}
	if _, statErr := os.Stat(accountDir); statErr != nil {
		return false, nil
	}
	slot, readErr := c.readCredentialAt("claude", accountDir, false)
	if IsCredentialMissing(readErr) {
		return true, nil
	}
	if readErr != nil {
		// Indeterminate slot state: without knowing whether the slot is
		// healthy, neither overwriting it nor discarding the orphan is
		// provably safe.
		return false, fmt.Errorf("provideraccounts: inspect slot %s before adoption: %w", accountID, readErr)
	}
	return c.credentialSignedOut("claude", slot.Data), nil
}

// removeEphemeralRemnants deletes whatever a crashed ephemeral home
// left behind: the darwin Keychain item named by the home's path hash
// (and any migrated credential file, via the seam's fallback) plus the
// leftover temporary directory. The caller has already proven the path
// sweepable.
func (c *Credentials) removeEphemeralRemnants(configHome string) error {
	var errs []error
	if runtime.GOOS == "darwin" {
		if err := c.keychain.remove(configHome, false); err != nil {
			errs = append(errs, err)
		}
	}
	if err := os.RemoveAll(configHome); err != nil {
		errs = append(errs, fmt.Errorf("provideraccounts: remove crashed ephemeral home %s: %w", configHome, err))
	}
	return errors.Join(errs...)
}
