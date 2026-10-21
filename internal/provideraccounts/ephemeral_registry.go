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
	"agent-overflow/internal/filelock"
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
// Each entry has a lease: a lock file beside it that the owning process
// holds until the home is cleaned up. The kernel releases it when that
// process exits, so a lease the sweep can take proves the owner is gone,
// whatever the entry's age, and a lease it cannot take marks a live owner.
//
// Each provider keeps its registry in its own shared home. One file per
// entry (not one shared file) so two concurrent app instances over the
// same home can never lose each other's records to a read-modify-write
// race: entries are independent, creation is atomic, and double-removal
// is a no-op.
const ephemeralRegistryDirName = "agent-overflow-ephemerals"

// unleasedEntryGrace applies only to entries without a lease file, which
// builds before leases wrote and may still be writing: probes finish in
// under a minute and a login flow in minutes, so an unleased entry older
// than an hour belongs to a crashed run.
const unleasedEntryGrace = time.Hour

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
	// healthy), "cleaned" (no credential remained), or "skipped" (the
	// home's owner is still running).
	Action string
	// Canonical reports that the orphan also replaced the canonical
	// credential: the owning account is the selected one and the canonical
	// store held the same login behind the orphan on its chain.
	Canonical bool
}

func (c *Credentials) ephemeralRegistryDir(providerName string) (string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return "", err
	}
	return filepath.Join(paths.SharedHome, ephemeralRegistryDirName), nil
}

func ephemeralRegistryKey(configHome string) string {
	sum := sha256.Sum256([]byte(configHome))
	return hex.EncodeToString(sum[:8])
}

func ephemeralRegistryFileName(configHome string) string {
	return ephemeralRegistryKey(configHome) + ".json"
}

func ephemeralLeaseFileName(configHome string) string {
	return ephemeralRegistryKey(configHome) + ".lock"
}

// registerEphemeralHome records a temporary provider home before any
// credential can exist in it and returns the lease the caller holds until
// the home is cleaned up. Ordering is the point: an unrecorded home that
// crashes is invisible forever, so registration failure fails the home's
// creation rather than proceeding uncovered. The lease is taken before the
// entry exists, so no sweep can see the entry without it.
func (c *Credentials) registerEphemeralHome(providerName, configHome, ownerAccountID string) (*os.File, error) {
	dir, err := c.ephemeralRegistryDir(providerName)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("provideraccounts: create ephemeral %s registry: %w", providerName, err)
	}
	leasePath := filepath.Join(dir, ephemeralLeaseFileName(configHome))
	lease, err := filelock.Open(leasePath)
	if err != nil {
		return nil, fmt.Errorf("provideraccounts: open ephemeral %s home lease: %w", providerName, err)
	}
	locked, err := filelock.TryLock(lease)
	if err != nil || !locked {
		_ = lease.Close()
		if err == nil {
			err = errors.New("held by another process")
		}
		return nil, fmt.Errorf("provideraccounts: take ephemeral %s home lease: %w", providerName, err)
	}
	entry := ephemeralRegistryEntry{
		ConfigHome: configHome,
		AccountID:  ownerAccountID,
		CreatedAt:  time.Now().UTC(),
	}
	if err := atomicfile.WriteJSON(filepath.Join(dir, ephemeralRegistryFileName(configHome)), entry); err != nil {
		return nil, errors.Join(
			fmt.Errorf("provideraccounts: record ephemeral %s home: %w", providerName, err),
			releaseEphemeralLease(lease, leasePath),
		)
	}
	return lease, nil
}

// unregisterEphemeralHome removes a home's entry, then ends its lease. The
// entry goes first so a sweep that takes the released lease finds no entry
// to act on. removed reports that the entry is gone and the lease consumed,
// even when ending the lease failed.
func (c *Credentials) unregisterEphemeralHome(providerName, configHome string, lease *os.File) (removed bool, err error) {
	dir, err := c.ephemeralRegistryDir(providerName)
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, ephemeralRegistryFileName(configHome))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("provideraccounts: clear ephemeral %s home record: %w", providerName, err)
	}
	return true, releaseEphemeralLease(lease, filepath.Join(dir, ephemeralLeaseFileName(configHome)))
}

// releaseEphemeralLease unlocks and closes a lease, then removes its file.
func releaseEphemeralLease(lease *os.File, leasePath string) error {
	var errs []error
	if lease != nil {
		if err := filelock.Release(lease); err != nil {
			errs = append(errs, fmt.Errorf("provideraccounts: release ephemeral home lease: %w", err))
		}
	}
	if err := os.Remove(leasePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("provideraccounts: remove ephemeral home lease: %w", err))
	}
	return errors.Join(errs...)
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
// selected maps each provider to its selected account ID.
//
// Per entry whose owner is gone: read whatever credential the home still
// holds (Keychain item for Claude on darwin, credential file otherwise).
// When the owning account is the selected one and the canonical store
// holds the same login behind the orphan on its chain, the orphan replaces
// it first, as CommitSelectedCredential orders a selected rotation. Then,
// if the owning account's slot still exists and the orphan's bytes are
// usable and the slot's are not (missing, the provider's sign-out husk, or
// behind the orphan on the rotation chain), restore them into the slot:
// the orphan may be the only live copy of a rotated chain. Then delete the
// remnant credential, the leftover directory, and the entry. Any step that
// cannot prove it is safe keeps the entry for the next boot instead of
// guessing.
func (c *Credentials) SweepEphemeralCredentials(now time.Time, selected map[string]string) ([]EphemeralSweepResult, error) {
	var results []EphemeralSweepResult
	var sweepErrs []error
	for _, providerName := range ephemeralProviders {
		providerResults, err := c.sweepEphemeralHomes(providerName, now, selected[providerName])
		results = append(results, providerResults...)
		if err != nil {
			sweepErrs = append(sweepErrs, err)
		}
	}
	return results, errors.Join(sweepErrs...)
}

func (c *Credentials) sweepEphemeralHomes(providerName string, now time.Time, selectedAccountID string) ([]EphemeralSweepResult, error) {
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
		skipped := EphemeralSweepResult{
			Provider:   providerName,
			ConfigHome: entry.ConfigHome,
			AccountID:  entry.AccountID,
			Action:     "skipped",
		}
		leasePath := filepath.Join(dir, ephemeralLeaseFileName(entry.ConfigHome))
		lease, claim, claimErr := claimEphemeralEntry(entryPath, leasePath, entry, now)
		if claimErr != nil {
			sweepErrs = append(sweepErrs, fmt.Errorf("provideraccounts: claim ephemeral %s home %s: %w", providerName, entry.ConfigHome, claimErr))
			continue
		}
		switch claim {
		case entryOwned:
			results = append(results, skipped)
			continue
		case entryGone:
			continue
		}

		result, entryErr := c.sweepOneEphemeralHome(providerName, entry, selectedAccountID)
		if entryErr != nil {
			// Kept for the next boot: the orphan may still hold the only
			// live copy of a chain, so an indeterminate state is never
			// resolved by deleting.
			sweepErrs = append(sweepErrs, entryErr)
			if lease != nil {
				if releaseErr := filelock.Release(lease); releaseErr != nil {
					sweepErrs = append(sweepErrs, releaseErr)
				}
			}
			continue
		}
		if removeErr := os.Remove(entryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			sweepErrs = append(sweepErrs, removeErr)
		}
		if releaseErr := releaseEphemeralLease(lease, leasePath); releaseErr != nil {
			sweepErrs = append(sweepErrs, releaseErr)
		}
		results = append(results, result)
	}
	return results, errors.Join(sweepErrs...)
}

type ephemeralClaim int

const (
	// entryClaimed: the owner is gone and the sweep may act on the entry.
	entryClaimed ephemeralClaim = iota
	// entryOwned: the owner is still running.
	entryOwned
	// entryGone: the owner cleaned up between the read and the claim.
	entryGone
)

// claimEphemeralEntry decides whether the sweep may act on an entry. A
// leased entry is claimed by taking its lease, which is then held until the
// sweep finishes with it, so two sweeps never act on one entry. An entry
// without a lease file is one an older build wrote, judged by its age.
func claimEphemeralEntry(entryPath, leasePath string, entry ephemeralRegistryEntry, now time.Time) (*os.File, ephemeralClaim, error) {
	lease, err := os.OpenFile(leasePath, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		if now.Sub(entry.CreatedAt) < unleasedEntryGrace {
			return nil, entryOwned, nil
		}
		return nil, entryClaimed, nil
	}
	if err != nil {
		return nil, entryClaimed, err
	}
	locked, err := filelock.TryLock(lease)
	if err != nil || !locked {
		_ = lease.Close()
		return nil, entryOwned, err
	}
	// The owner removes its entry before releasing the lease, so an entry
	// still present under the lease is one its owner never cleaned up.
	if _, err := os.Stat(entryPath); err != nil {
		releaseErr := filelock.Release(lease)
		if errors.Is(err, os.ErrNotExist) {
			return nil, entryGone, releaseErr
		}
		return nil, entryClaimed, errors.Join(err, releaseErr)
	}
	return lease, entryClaimed, nil
}

func (c *Credentials) sweepOneEphemeralHome(providerName string, entry ephemeralRegistryEntry, selectedAccountID string) (EphemeralSweepResult, error) {
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
		if entry.AccountID == selectedAccountID {
			replaced, err := c.adoptIntoCanonical(providerName, orphan.Bytes())
			if err != nil {
				return result, err
			}
			result.Canonical = replaced
		}
		adopted, err := c.adoptIntoSlot(providerName, entry.AccountID, orphan.Bytes())
		if err != nil {
			return result, err
		}
		if adopted || result.Canonical {
			result.Action = "adopted"
		}
	}
	return result, c.removeEphemeralRemnants(providerName, entry.ConfigHome)
}

// adoptIntoCanonical replaces the canonical credential with the selected
// account's orphan when the canonical store holds the same login
// (Policy.LoginID) strictly behind it on the chain. A different login there
// is an outside sign-in, and unknown identity or position proves nothing,
// so either leaves the canonical store alone.
//
// Nothing serializes this read-compare-write against a provider CLI writing
// the canonical store itself. The rotation chain does: the canonical bytes
// behind the orphan hold a refresh token the orphan's rotation consumed, so
// no CLI can advance them, and a sign-in lands a different login or a later
// position.
func (c *Credentials) adoptIntoCanonical(providerName string, orphan []byte) (bool, error) {
	if c.policy.LoginID == nil {
		return false, nil
	}
	canonical, err := c.ReadCredentialSnapshot(providerName, "", true)
	if IsCredentialMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("provideraccounts: inspect canonical %s credential before adoption: %w", providerName, err)
	}
	canonicalLogin, ok := c.policy.LoginID(providerName, canonical.Bytes())
	if !ok {
		return false, nil
	}
	orphanLogin, ok := c.policy.LoginID(providerName, orphan)
	if !ok || orphanLogin != canonicalLogin || !c.chainAdvances(providerName, canonical.Bytes(), orphan) {
		return false, nil
	}
	if err := c.writeActiveCredential(providerName, orphan); err != nil {
		return false, fmt.Errorf("provideraccounts: restore crash-orphaned %s credential to the canonical home: %w", providerName, err)
	}
	return true, nil
}

// adoptIntoSlot restores the orphan into the account's slot when the slot
// would gain from it: the slot directory still exists (the account is
// still saved on this machine — the prune runs first) and its credential
// is missing, the provider's own sign-out husk, or strictly behind the
// orphan on the rotation chain. A slot at or past the orphan's position,
// or one whose position is unknown, is never overwritten: the user may
// have re-logged-in since the crash, and those bytes are newer than any
// orphan. The slot lock is held from the read to the write, so another
// process's slot write lands wholly before the decision or after it.
func (c *Credentials) adoptIntoSlot(providerName, accountID string, orphan []byte) (adopted bool, retErr error) {
	accountDir, err := c.accountDirectory(providerName, accountID)
	if err != nil {
		return false, nil
	}
	if _, statErr := os.Stat(accountDir); statErr != nil {
		return false, nil
	}
	lock, err := lockSlot(accountDir)
	if err != nil {
		return false, err
	}
	defer func() { retErr = errors.Join(retErr, releaseSlot(lock)) }()
	slot, readErr := c.readCredentialAt(providerName, accountDir, false)
	switch {
	case IsCredentialMissing(readErr):
	case readErr != nil:
		// Indeterminate slot state: without knowing whether the slot is
		// healthy, neither overwriting it nor discarding the orphan is
		// provably safe.
		return false, fmt.Errorf("provideraccounts: inspect %s slot %s before adoption: %w", providerName, accountID, readErr)
	case c.credentialSignedOut(providerName, slot.Bytes()):
	case !c.chainAdvances(providerName, slot.Bytes(), orphan):
		return false, nil
	}
	if err := c.writeCredentialAt(providerName, accountDir, false, orphan); err != nil {
		return false, fmt.Errorf("provideraccounts: restore crash-orphaned %s credential to slot %s: %w", providerName, accountID, err)
	}
	return true, nil
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
