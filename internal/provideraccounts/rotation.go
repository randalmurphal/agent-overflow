package provideraccounts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// A rotation is a temporary home's credential that holds another refresh
// token than the one the home was seeded with: the CLI spent the seed's
// token and the home holds its only successor. settleRotation is the one
// writer that places it, for a live usage probe as it finishes and for the
// boot sweep after a crash, and the one judge of when the home may go.
//
// It places the rotation by descent alone, never by whose login anything
// says it is:
//
//   - Every durable store that holds the seed's token takes the successor:
//     the canonical credential when this store selected the owner (only that
//     store writes it), and every saved slot. A store holding a token holds
//     that login, so its successor is the same login moved forward, whatever
//     account the slot belongs to.
//   - The owner's slot takes it when it still holds what it held when the
//     probe started (ephemeralProvenance.SlotToken). The seed of a home this
//     build creates is always its owner's login, proven when it was seeded:
//     an inactive account's is its slot, a selected account's the canonical
//     credential the app recorded for it.
//
// Each slot write first records the seed's token as consumed (consumed.go).
// A rotation nothing took is deleted only on evidence that nothing needs it
// (rotationStillNeeded); otherwise the home and its entry are kept.

// ephemeralProvenance is what a temporary home started from.
type ephemeralProvenance struct {
	// AccountID owns the home; empty for a sign-in home.
	AccountID string
	// SeedToken fingerprints the seed's refresh token (TokenFingerprint).
	SeedToken string
	// SlotToken fingerprints the refresh token the owner's slot held when
	// the home was seeded; empty in entries older builds wrote.
	SlotToken string
}

// RotationLanding says where a temporary home's rotation went.
type RotationLanding struct {
	// Rotated reports that the home holds a rotation at all.
	Rotated bool
	// Canonical reports that the rotation replaced the canonical credential.
	Canonical bool
	// Slots names the saved slots that took the rotation.
	Slots []string
	// Kept reports that nothing took the rotation and nothing proves it
	// unneeded: the home stays, with its entry, for the next boot's sweep.
	Kept bool
}

// placed reports that a durable store took the rotation.
func (l RotationLanding) placed() bool { return l.Canonical || len(l.Slots) > 0 }

// settleRotation places a temporary home's credential, orphan, by the rules
// above. selectedHere reports that this store selected the owner. An error
// leaves the home to be kept: the state could not be judged.
func (c *Credentials) settleRotation(providerName string, from ephemeralProvenance, orphan []byte, selectedHere bool) (RotationLanding, error) {
	orphanToken, hasToken := c.tokenFingerprint(providerName, orphan)
	if from.AccountID == "" || from.SeedToken == "" || !hasToken || orphanToken == from.SeedToken ||
		c.credentialSignedOut(providerName, orphan) {
		// Nothing was consumed: a sign-in home, a seed without a refresh
		// token, or a home that never rotated. The seed's token is as live as
		// wherever it came from. A sign-out the CLI left there is no login.
		return RotationLanding{}, nil
	}
	landing := RotationLanding{Rotated: true}
	if selectedHere {
		replaced, err := c.replaceActiveCredentialHolding(providerName, from.SeedToken, orphan)
		if err != nil {
			return landing, fmt.Errorf("provideraccounts: restore a refreshed %s login to the canonical home: %w", providerName, err)
		}
		landing.Canonical = replaced
	}
	slots, err := c.savedSlotIDs(providerName)
	if err != nil {
		return landing, err
	}
	// The owner's slot is always inspected, whatever the listing shows: one
	// that cannot be read keeps the rotation.
	if !slices.Contains(slots, from.AccountID) {
		slots = append(slots, from.AccountID)
	}
	for _, accountID := range slots {
		slotToken := ""
		if accountID == from.AccountID {
			slotToken = from.SlotToken
		}
		took, err := c.adoptIntoSlot(providerName, accountID, from.SeedToken, slotToken, orphan)
		if err != nil {
			return landing, err
		}
		if took {
			landing.Slots = append(landing.Slots, accountID)
		}
	}
	if landing.placed() {
		return landing, nil
	}
	needed, err := c.rotationStillNeeded(providerName, from, orphanToken, selectedHere)
	if err != nil {
		return landing, err
	}
	landing.Kept = needed
	return landing, nil
}

// savedSlotIDs lists the account slots of one provider. Entries that are not
// valid account ids are no slot this package wrote, and are skipped.
func (c *Credentials) savedSlotIDs(providerName string) ([]string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(paths.SharedHome, accountDirectoryName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("provideraccounts: list %s account slots: %w", providerName, err)
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() && safeAccountID.MatchString(entry.Name()) {
			ids = append(ids, entry.Name())
		}
	}
	return ids, nil
}

// adoptIntoSlot writes a rotation into one slot when the slot holds the
// seed's token, or slotToken when that is set: what the owner's slot held
// when the probe started. Anything else in the slot (another login, a later
// rotation, a sign-in since, nothing at all) is left alone. The slot lock is
// held from the check to the write, so another process's slot write or
// removal lands wholly before the decision or after it. A slot that cannot
// be inspected is an error, never a slot that does not exist.
func (c *Credentials) adoptIntoSlot(providerName, accountID, seed, slotToken string, orphan []byte) (adopted bool, retErr error) {
	accountDir, err := c.accountDirectory(providerName, accountID)
	if err != nil {
		return false, err
	}
	lock, err := c.lockSlot(providerName, accountID)
	if err != nil {
		return false, err
	}
	defer func() { retErr = errors.Join(retErr, releaseSlot(lock)) }()
	slot, present, err := c.inspectSlotLocked(providerName, accountID, accountDir)
	if err != nil || !present {
		return false, err
	}
	if !c.holdsConsumed(providerName, slot.Bytes(), seed) &&
		(slotToken == "" || !c.holdsConsumed(providerName, slot.Bytes(), slotToken)) {
		return false, nil
	}
	if err := c.writeConsumedMarker(providerName, accountID, seed); err != nil {
		return false, err
	}
	if err := c.writeCredentialAt(providerName, accountDir, false, orphan); err != nil {
		return false, fmt.Errorf("provideraccounts: save a refreshed %s login to slot %s: %w", providerName, accountID, err)
	}
	return true, nil
}

// inspectSlotLocked reads a slot for a decision. present is false only when
// the slot's credential does not exist, its directory included. The caller
// holds the slot lock.
func (c *Credentials) inspectSlotLocked(providerName, accountID, accountDir string) (CredentialSnapshot, bool, error) {
	slot, err := c.readCredentialAt(providerName, accountDir, false)
	if IsCredentialMissing(err) {
		return CredentialSnapshot{}, false, nil
	}
	if err != nil {
		return CredentialSnapshot{}, false, fmt.Errorf("provideraccounts: inspect %s slot %s: %w", providerName, accountID, err)
	}
	return slot, true, nil
}

// rotationStillNeeded reports whether a rotation nothing took may still be
// the only live copy of its login's chain. It answers false only on
// evidence that nothing needs it:
//
//   - the canonical credential or the owner's slot already holds it;
//   - the owner's slot holds neither the seed's token nor the token it held
//     when the probe started (only entries that recorded SlotToken can say
//     so): something proven wrote the slot since, a sign-in, a switch or a
//     landing, and the account's login lives on there.
//
// The canonical credential still holding the seed's token, when another
// store selected the owner, is a store that will restore it: kept. A slot
// that is missing (the owner removed, or another data root pruned it) or
// cannot be read is no evidence either: kept.
func (c *Credentials) rotationStillNeeded(providerName string, from ephemeralProvenance, orphanToken string, selectedHere bool) (bool, error) {
	canonical, err := c.ReadCredentialSnapshot(providerName, "", true)
	switch {
	case IsCredentialMissing(err):
	case err != nil:
		return true, fmt.Errorf("provideraccounts: inspect canonical %s credential: %w", providerName, err)
	case c.holdsConsumed(providerName, canonical.Bytes(), orphanToken):
		return false, nil
	case !selectedHere && c.holdsConsumed(providerName, canonical.Bytes(), from.SeedToken):
		return true, nil
	}
	accountDir, err := c.accountDirectory(providerName, from.AccountID)
	if err != nil {
		return true, err
	}
	lock, err := c.lockSlot(providerName, from.AccountID)
	if err != nil {
		return true, err
	}
	slot, present, err := c.inspectSlotLocked(providerName, from.AccountID, accountDir)
	err = errors.Join(err, releaseSlot(lock))
	if err != nil || !present {
		return true, err
	}
	if c.holdsConsumed(providerName, slot.Bytes(), orphanToken) {
		return false, nil
	}
	return from.SlotToken == "", nil
}
