package provideraccounts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// removed once its rotation is settled (rotation.go), and the boot sweep
// settles whatever a crash left behind by the same rule.
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
var ephemeralProviders = []string{"claude", "codex", "agy"}

// ephemeralHomePrefix is the os.MkdirTemp prefix newEphemeralHome uses for
// one provider's homes. The sweep refuses to act on any recorded path
// without it: a corrupted or foreign registry entry must never aim the
// remnant removal at a real directory.
func ephemeralHomePrefix(providerName string) string {
	return "agent-overflow-" + providerName + "-"
}

type ephemeralRegistryEntry struct {
	ConfigHome string `json:"configHome"`
	AccountID  string `json:"accountId,omitempty"`
	// SeedDigest is the fingerprint of the refresh token the home was seeded
	// with (consumed.go); empty for a login home, which starts with none.
	SeedDigest string `json:"seedDigest,omitempty"`
	// SlotToken is the fingerprint of the refresh token the owner's slot held
	// when the home was seeded (ephemeralProvenance). Entries written before
	// it was recorded have none.
	SlotToken string    `json:"slotToken,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// Kept records that a sweep already reported keeping this entry, so a
	// later sweep that keeps it again reports nothing.
	Kept bool `json:"kept,omitempty"`
}

// EphemeralSweepResult reports what the boot sweep did with one
// crash-orphaned ephemeral home.
type EphemeralSweepResult struct {
	Provider   string
	ConfigHome string
	AccountID  string
	// Action is one of "adopted" (the orphan held a rotation and the
	// canonical store or a slot took it, settleRotation), "discarded" (a
	// credential was left behind that nothing needs), "kept" (a rotation
	// nothing took and nothing proves unneeded; reported once), "cleaned"
	// (no credential remained), or "skipped" (the home's owner is still
	// running).
	Action string
	// Canonical reports that the orphan also replaced the canonical
	// credential: the owning account is this store's selected one and the
	// canonical store still held the token the orphan's rotation consumed.
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
func (c *Credentials) registerEphemeralHome(providerName, configHome string, from ephemeralProvenance) (*os.File, error) {
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
		AccountID:  from.AccountID,
		SeedDigest: from.SeedToken,
		SlotToken:  from.SlotToken,
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

// SweepEphemeralCredentials settles credentials left behind by ephemeral
// homes that never reached their cleanup: a crash or kill mid-probe or
// mid-login. Run at boot, after the orphan-slot prune. selected maps each
// provider to this store's selected account ID.
//
// Per entry whose owner is gone: read whatever credential the home still
// holds (Keychain item for Claude on darwin, credential file otherwise) and
// settle it as the live probe would have (settleRotation). A rotation that
// is placed, or that nothing needs, is deleted with its directory and entry.
// One that is still needed, or whose stores cannot be read, keeps its entry
// for the next boot; a kept rotation is reported once, and ends when a later
// sweep can place or discard it or when its account is removed
// (ForgetAccountEphemerals).
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

// beforeEphemeralClaimForTest, when a test sets it, runs between a sweep's
// read of an entry and its claim of the entry's lease.
var beforeEphemeralClaimForTest func()

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
		if !dirEntry.IsDir() && strings.HasSuffix(dirEntry.Name(), ".lock") {
			if err := removeStrandedLease(dir, dirEntry.Name(), now); err != nil {
				sweepErrs = append(sweepErrs, err)
			}
			continue
		}
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
		if beforeEphemeralClaimForTest != nil {
			beforeEphemeralClaimForTest()
		}
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
		case entryClaimed:
		}
		// Decided from the entry as it stands under the lease: another sweep
		// may have settled or marked it between the read above and the claim.
		if found, err := atomicfile.ReadJSON(entryPath, &entry); err != nil || !found {
			if err != nil {
				sweepErrs = append(sweepErrs, fmt.Errorf("provideraccounts: reread ephemeral %s registry entry %s: %w", providerName, dirEntry.Name(), err))
			}
			if lease != nil {
				if releaseErr := filelock.Release(lease); releaseErr != nil {
					sweepErrs = append(sweepErrs, releaseErr)
				}
			}
			continue
		}

		result, entryErr := c.sweepOneEphemeralHome(providerName, entry, selectedAccountID)
		if entryErr != nil || result.Action == "kept" {
			// Kept for the next boot: the orphan may still hold the only
			// live copy of a chain, so an indeterminate state is never
			// resolved by deleting.
			if entryErr != nil {
				sweepErrs = append(sweepErrs, entryErr)
			} else if !entry.Kept {
				entry.Kept = true
				if err := atomicfile.WriteJSON(entryPath, entry); err != nil {
					sweepErrs = append(sweepErrs, fmt.Errorf("provideraccounts: mark ephemeral %s home %s kept: %w", providerName, entry.ConfigHome, err))
				}
				results = append(results, result)
			}
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

// removeStrandedLease deletes a lease file whose entry is gone, left when
// a cleanup could not delete it (Windows refuses to delete a file another
// process has open). A lease younger than unleasedEntryGrace may belong to
// a registration that has not written its entry yet, and a held one to an
// owner still cleaning up; both are left alone.
func removeStrandedLease(dir, name string, now time.Time) error {
	leasePath := filepath.Join(dir, name)
	if _, err := os.Stat(filepath.Join(dir, strings.TrimSuffix(name, ".lock")+".json")); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	info, err := os.Stat(leasePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provideraccounts: inspect stranded ephemeral lease %s: %w", name, err)
	}
	if now.Sub(info.ModTime()) < unleasedEntryGrace {
		return nil
	}
	lease, err := os.OpenFile(leasePath, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provideraccounts: open stranded ephemeral lease %s: %w", name, err)
	}
	locked, err := filelock.TryLock(lease)
	if err != nil || !locked {
		_ = lease.Close()
		return err
	}
	return releaseEphemeralLease(lease, leasePath)
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
	from := ephemeralProvenance{AccountID: entry.AccountID, SeedToken: entry.SeedDigest, SlotToken: entry.SlotToken}
	selectedHere := entry.AccountID != "" && entry.AccountID == selectedAccountID
	landing, err := c.settleRotation(providerName, from, orphan.Bytes(), selectedHere)
	if err != nil {
		return result, err
	}
	result.Canonical = landing.Canonical
	switch {
	case landing.Kept:
		result.Action = "kept"
		return result, nil
	case landing.placed():
		result.Action = "adopted"
	default:
		result.Action = "discarded"
	}
	return result, c.removeEphemeralRemnants(providerName, entry.ConfigHome)
}

// ForgetAccountEphemerals deletes the entries, homes and credentials of a
// removed account's temporary homes whose owners are gone: a kept rotation
// ends with its account. A home whose owner still runs is left to it. The
// caller removed the account; account removal calls this, the orphan-slot
// prune does not (a slot it prunes may be another data root's account).
func (c *Credentials) ForgetAccountEphemerals(providerName, accountID string, now time.Time) error {
	dir, err := c.ephemeralRegistryDir(providerName)
	if err != nil {
		return err
	}
	dirEntries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provideraccounts: read ephemeral %s registry: %w", providerName, err)
	}
	var errs []error
	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() || !strings.HasSuffix(dirEntry.Name(), ".json") {
			continue
		}
		entryPath := filepath.Join(dir, dirEntry.Name())
		var entry ephemeralRegistryEntry
		found, readErr := atomicfile.ReadJSON(entryPath, &entry)
		if readErr != nil || !found || entry.AccountID != accountID || !sweepableEphemeralHome(providerName, entry.ConfigHome) {
			continue
		}
		leasePath := filepath.Join(dir, ephemeralLeaseFileName(entry.ConfigHome))
		lease, claim, claimErr := claimEphemeralEntry(entryPath, leasePath, entry, now)
		if claimErr != nil {
			errs = append(errs, fmt.Errorf("provideraccounts: claim ephemeral %s home %s: %w", providerName, entry.ConfigHome, claimErr))
			continue
		}
		if claim != entryClaimed {
			continue
		}
		if err := c.removeEphemeralRemnants(providerName, entry.ConfigHome); err != nil {
			errs = append(errs, err, releaseLease(lease))
			continue
		}
		if err := os.Remove(entryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		errs = append(errs, releaseEphemeralLease(lease, leasePath))
	}
	return errors.Join(errs...)
}

// releaseLease releases a claimed lease, if any, keeping its file.
func releaseLease(lease *os.File) error {
	if lease == nil {
		return nil
	}
	return filelock.Release(lease)
}

// removeEphemeralRemnants deletes whatever a crashed ephemeral home left
// behind: for Claude on darwin the Keychain item named by the home's path
// hash (and any migrated credential file, via the seam's fallback), plus
// the leftover temporary directory. The caller has already proven the
// path sweepable.
func (c *Credentials) removeEphemeralRemnants(providerName, configHome string) error {
	var errs []error
	if kc, ok := c.keychainFor(providerName); ok {
		if err := kc.remove(configHome, false); err != nil {
			errs = append(errs, err)
		}
	}
	if err := os.RemoveAll(configHome); err != nil {
		errs = append(errs, fmt.Errorf("provideraccounts: remove crashed ephemeral %s home %s: %w", providerName, configHome, err))
	}
	return errors.Join(errs...)
}
