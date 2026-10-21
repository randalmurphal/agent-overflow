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

// The ephemeral registry closes the crash window on temporary provider
// homes. Every ephemeral home holds a credential seeded by AO or written
// by the CLI (for Claude on darwin, a hash-named Keychain item), and a
// probe there can ROTATE the account's refresh chain. A crash between the
// rotation and the read-back leaves the only live copy of that chain in a
// store nothing references, and an abandoned home keeps a live credential
// in the temporary directory indefinitely. Each home is therefore recorded
// in a per-entry file BEFORE any credential can exist there, the entry is
// removed on clean teardown, and the boot sweep recovers whatever a crash
// left behind: adopt the bytes into the owning slot when the slot is dead
// or behind the orphan on its chain, then delete the remnant.
//
// Each provider keeps its registry in its own shared home. One file per
// entry (not one shared file) so two concurrent app instances over the
// same home can never lose each other's records to a read-modify-write
// race: entries are independent, creation is atomic, and double-removal
// is a no-op.
const ephemeralRegistryDirName = "agent-overflow-ephemerals"

// ephemeralSweepGrace keeps the sweep off entries another live instance
// may still be using: probes finish in under a minute and a login flow
// in minutes, so anything older than an hour belongs to a crashed run.
// A younger entry just waits for a later boot.
const ephemeralSweepGrace = time.Hour

// ephemeralProviders are the providers whose temporary homes the registry
// covers, in sweep order.
var ephemeralProviders = []string{"claude", "codex"}

// ephemeralHomePrefix is the os.MkdirTemp prefix newEphemeralHome uses for
// one provider's homes. The sweep refuses to act on any recorded path
// without it: a corrupted or foreign registry entry must never aim the
// remnant removal at a real directory.
func ephemeralHomePrefix(providerName string) string {
	return "agent-overflow-" + providerName + "-"
}

type ephemeralRegistryEntry struct {
	ConfigHome string    `json:"configHome"`
	AccountID  string    `json:"accountId,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// EphemeralSweepResult reports what the boot sweep did with one
// crash-orphaned ephemeral home.
type EphemeralSweepResult struct {
	Provider   string
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

func (c *Credentials) ephemeralRegistryDir(providerName string) (string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return "", err
	}
	return filepath.Join(paths.SharedHome, ephemeralRegistryDirName), nil
}

func ephemeralRegistryFileName(configHome string) string {
	sum := sha256.Sum256([]byte(configHome))
	return hex.EncodeToString(sum[:8]) + ".json"
}

// registerEphemeralHome records a temporary provider home before any
// credential can exist in it. Ordering is the point: an unrecorded home
// that crashes is invisible forever, so registration failure fails the
// home's creation rather than proceeding uncovered.
func (c *Credentials) registerEphemeralHome(providerName, configHome, ownerAccountID string) error {
	dir, err := c.ephemeralRegistryDir(providerName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("provideraccounts: create ephemeral %s registry: %w", providerName, err)
	}
	entry := ephemeralRegistryEntry{
		ConfigHome: configHome,
		AccountID:  ownerAccountID,
		CreatedAt:  time.Now().UTC(),
	}
	if err := atomicfile.WriteJSON(filepath.Join(dir, ephemeralRegistryFileName(configHome)), entry); err != nil {
		return fmt.Errorf("provideraccounts: record ephemeral %s home: %w", providerName, err)
	}
	return nil
}

func (c *Credentials) unregisterEphemeralHome(providerName, configHome string) error {
	dir, err := c.ephemeralRegistryDir(providerName)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ephemeralRegistryFileName(configHome))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("provideraccounts: clear ephemeral %s home record: %w", providerName, err)
	}
	return nil
}

// sweepableEphemeralHome is the structural guard on recorded paths: the
// sweep only ever acts on absolute paths whose directory name carries
// the provider's ephemeral MkdirTemp prefix, so no registry content —
// corrupt, stale, or hostile — can point the removal or the
// Keychain-service hash at the canonical home, a slot, or any other real
// location.
func sweepableEphemeralHome(providerName, configHome string) bool {
	return filepath.IsAbs(configHome) &&
		strings.HasPrefix(filepath.Base(configHome), ephemeralHomePrefix(providerName))
}

// SweepEphemeralCredentials recovers and removes credentials left behind
// by ephemeral homes that never reached their cleanup — a crash or kill
// mid-probe or mid-login. Run at boot, after the orphan-slot prune.
//
// Per entry older than the grace period: read whatever credential the
// home still holds (Keychain item for Claude on darwin, credential file
// otherwise); if the owning account's slot still exists and the orphan's
// bytes are usable and the slot's are not (missing, the provider's
// sign-out husk, or behind the orphan on the rotation chain), restore
// them into the slot — the orphan may be the only live copy of a rotated
// chain. Then delete the remnant credential, the leftover directory, and
// the entry. Any step that cannot prove it is safe keeps the entry for
// the next boot instead of guessing.
func (c *Credentials) SweepEphemeralCredentials(now time.Time) ([]EphemeralSweepResult, error) {
	var results []EphemeralSweepResult
	var sweepErrs []error
	for _, providerName := range ephemeralProviders {
		providerResults, err := c.sweepEphemeralHomes(providerName, now)
		results = append(results, providerResults...)
		if err != nil {
			sweepErrs = append(sweepErrs, err)
		}
	}
	return results, errors.Join(sweepErrs...)
}

func (c *Credentials) sweepEphemeralHomes(providerName string, now time.Time) ([]EphemeralSweepResult, error) {
	dir, err := c.ephemeralRegistryDir(providerName)
	if err != nil {
		return nil, err
	}
	dirEntries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("provideraccounts: read ephemeral %s registry: %w", providerName, err)
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
		if readErr != nil || !found || !sweepableEphemeralHome(providerName, entry.ConfigHome) {
			// Unreadable or unsafe entries can never become actionable;
			// keeping them would re-report forever.
			if readErr != nil {
				sweepErrs = append(sweepErrs, fmt.Errorf("provideraccounts: drop unreadable ephemeral %s registry entry %s: %w", providerName, dirEntry.Name(), readErr))
			}
			if removeErr := os.Remove(entryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				sweepErrs = append(sweepErrs, removeErr)
			}
			continue
		}
		if now.Sub(entry.CreatedAt) < ephemeralSweepGrace {
			results = append(results, EphemeralSweepResult{
				Provider:   providerName,
				ConfigHome: entry.ConfigHome,
				AccountID:  entry.AccountID,
				Action:     "skipped",
			})
			continue
		}

		result, entryErr := c.sweepOneEphemeralHome(providerName, entry)
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

func (c *Credentials) sweepOneEphemeralHome(providerName string, entry ephemeralRegistryEntry) (EphemeralSweepResult, error) {
	result := EphemeralSweepResult{Provider: providerName, ConfigHome: entry.ConfigHome, AccountID: entry.AccountID}

	orphan, readErr := c.readCredentialAt(providerName, entry.ConfigHome, false)
	if readErr != nil && !IsCredentialMissing(readErr) {
		return result, fmt.Errorf("provideraccounts: read crash-orphaned ephemeral %s credential at %s: %w", providerName, entry.ConfigHome, readErr)
	}
	if IsCredentialMissing(readErr) {
		result.Action = "cleaned"
		return result, c.removeEphemeralRemnants(providerName, entry.ConfigHome)
	}

	result.Action = "discarded"
	if entry.AccountID != "" && !c.credentialSignedOut(providerName, orphan.Bytes()) {
		adopt, decideErr := c.slotNeedsAdoption(providerName, entry.AccountID, orphan.Bytes())
		if decideErr != nil {
			return result, decideErr
		}
		if adopt {
			if writeErr := c.WriteAccountCredential(providerName, entry.AccountID, orphan.Bytes()); writeErr != nil {
				return result, fmt.Errorf("provideraccounts: restore crash-orphaned %s credential to slot %s: %w", providerName, entry.AccountID, writeErr)
			}
			result.Action = "adopted"
		}
	}
	return result, c.removeEphemeralRemnants(providerName, entry.ConfigHome)
}

// slotNeedsAdoption reports whether the account's slot would gain from
// the orphan's bytes: the slot directory still exists (the account is
// still saved on this machine — the prune runs first) and its credential
// is missing, the provider's own sign-out husk, or strictly behind the
// orphan on the rotation chain. A slot at or past the orphan's position,
// or one whose position is unknown, is never overwritten: the user may
// have re-logged-in since the crash, and those bytes are newer than any
// orphan.
func (c *Credentials) slotNeedsAdoption(providerName, accountID string, orphan []byte) (bool, error) {
	accountDir, err := c.accountDirectory(providerName, accountID)
	if err != nil {
		return false, nil
	}
	if _, statErr := os.Stat(accountDir); statErr != nil {
		return false, nil
	}
	slot, readErr := c.readCredentialAt(providerName, accountDir, false)
	if IsCredentialMissing(readErr) {
		return true, nil
	}
	if readErr != nil {
		// Indeterminate slot state: without knowing whether the slot is
		// healthy, neither overwriting it nor discarding the orphan is
		// provably safe.
		return false, fmt.Errorf("provideraccounts: inspect %s slot %s before adoption: %w", providerName, accountID, readErr)
	}
	if c.credentialSignedOut(providerName, slot.Bytes()) {
		return true, nil
	}
	return c.chainAdvances(providerName, slot.Bytes(), orphan), nil
}

// chainAdvances reports that next sits strictly later than current on one
// account's rotation chain. Bytes without an ordering signal never
// advance anything.
func (c *Credentials) chainAdvances(providerName string, current, next []byte) bool {
	if c.policy.ChainPosition == nil {
		return false
	}
	currentPosition, ok := c.policy.ChainPosition(providerName, current)
	if !ok {
		return false
	}
	nextPosition, ok := c.policy.ChainPosition(providerName, next)
	return ok && nextPosition > currentPosition
}

// removeEphemeralRemnants deletes whatever a crashed ephemeral home left
// behind: for Claude on darwin the Keychain item named by the home's path
// hash (and any migrated credential file, via the seam's fallback), plus
// the leftover temporary directory. The caller has already proven the
// path sweepable.
func (c *Credentials) removeEphemeralRemnants(providerName, configHome string) error {
	var errs []error
	if runtime.GOOS == "darwin" && providerName == "claude" {
		if err := c.keychain.remove(configHome, false); err != nil {
			errs = append(errs, err)
		}
	}
	if err := os.RemoveAll(configHome); err != nil {
		errs = append(errs, fmt.Errorf("provideraccounts: remove crashed ephemeral %s home %s: %w", providerName, configHome, err))
	}
	return errors.Join(errs...)
}
