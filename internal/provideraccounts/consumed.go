package provideraccounts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
)

// Consumed-credential markers: the exact provenance of a crash recovery.
//
// When the boot sweep restores a crashed probe's rotation into a slot, the
// bytes the probe started from hold a refresh token that rotation used up.
// Copies of those bytes outlive the crash: the canonical credential, when
// the probe was of the selected account, and any snapshot a running
// backend read before the recovery landed. So the sweep records their
// digest beside the slot's lock before it writes the slot, and every later
// slot write of exactly those bytes is dropped. They can never authorize
// anything again, and the slot holds their live successor. While the
// canonical store still holds them, the store that selected the account
// promotes the slot into it (PromoteSlotOverCanonical).
//
// Dropping such a write is the safety property; the promotion only restores
// a working canonical credential sooner. The marker holds a SHA-256 of the
// credential bytes: one-way, and not itself a credential. It lives as long
// as the account's slot does.

const consumedMarkerSuffix = ".consumed"

func credentialDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *Credentials) consumedMarkerPath(providerName, accountID string) (string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return "", err
	}
	if !safeAccountID.MatchString(accountID) {
		return "", fmt.Errorf("provideraccounts: invalid account id %q", accountID)
	}
	return filepath.Join(paths.SharedHome, accountLockDirectoryName, accountID+consumedMarkerSuffix), nil
}

// readConsumedMarker returns the digest recorded for a slot, or "" when
// none is. Any other failure is returned: a marker that cannot be read
// cannot be honored, so the caller must not write the slot. The caller
// holds the slot lock.
func (c *Credentials) readConsumedMarker(providerName, accountID string) (string, error) {
	path, err := c.consumedMarkerPath(providerName, accountID)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("provideraccounts: read %s slot %s recovery record: %w", providerName, accountID, err)
	}
	digest := string(data)
	if _, decodeErr := hex.DecodeString(digest); decodeErr != nil || len(digest) != sha256.Size*2 {
		return "", fmt.Errorf("provideraccounts: %s slot %s recovery record is malformed", providerName, accountID)
	}
	return digest, nil
}

// writeConsumedMarker records the digest of credential bytes a recovered
// rotation consumed. The caller holds the slot lock.
func (c *Credentials) writeConsumedMarker(providerName, accountID, digest string) error {
	path, err := c.consumedMarkerPath(providerName, accountID)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(path, []byte(digest)); err != nil {
		return fmt.Errorf("provideraccounts: record %s slot %s recovery: %w", providerName, accountID, err)
	}
	return nil
}

// removeConsumedMarker forgets a slot's recovery record with the slot. The
// caller holds the slot lock.
func (c *Credentials) removeConsumedMarker(providerName, accountID string) error {
	path, err := c.consumedMarkerPath(providerName, accountID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("provideraccounts: remove %s slot %s recovery record: %w", providerName, accountID, err)
	}
	return nil
}

// consumedSlotWrite reports whether data are the bytes a recovery recorded
// as consumed for this slot, logging the dropped write when they are. The
// caller holds the slot lock.
func (c *Credentials) consumedSlotWrite(providerName, accountID string, data []byte) (bool, error) {
	digest, err := c.readConsumedMarker(providerName, accountID)
	if err != nil || digest == "" || digest != credentialDigest(data) {
		return false, err
	}
	log.Printf("provideraccounts: kept %s slot %s: the write held the credential a recovered rotation consumed", providerName, accountID)
	return true, nil
}

// PromoteSlotOverCanonical puts an account's saved credential into the
// canonical store while the canonical store still holds the bytes a
// recovered rotation consumed. Another store's boot sweep leaves that state:
// it restores the rotation into the slot and never writes the canonical
// credential. Only the store that selected the account calls this, under
// its provider reconcile lock. It returns the credential it wrote, if any.
func (c *Credentials) PromoteSlotOverCanonical(providerName, accountID string) (CredentialSnapshot, bool, error) {
	consumed, slot, err := c.recoveredSlot(providerName, accountID)
	if err != nil || consumed == "" {
		return CredentialSnapshot{}, false, err
	}
	canonical, err := c.ReadCredentialSnapshot(providerName, "", true)
	if IsCredentialMissing(err) {
		return CredentialSnapshot{}, false, nil
	}
	if err != nil {
		return CredentialSnapshot{}, false, fmt.Errorf("provideraccounts: read canonical %s credential before promoting slot %s: %w", providerName, accountID, err)
	}
	if credentialDigest(canonical.Bytes()) != consumed {
		return CredentialSnapshot{}, false, nil
	}
	if err := c.writeActiveCredential(providerName, slot.Bytes()); err != nil {
		return CredentialSnapshot{}, false, fmt.Errorf("provideraccounts: restore %s slot %s to the canonical home: %w", providerName, accountID, err)
	}
	return slot, true, nil
}

// recoveredSlot reads a slot's recovery record and credential together
// under the slot lock. consumed is "" when there is nothing to promote: no
// record, no credential, or the slot itself on the consumed bytes.
func (c *Credentials) recoveredSlot(providerName, accountID string) (consumed string, slot CredentialSnapshot, retErr error) {
	accountDir, err := c.accountDirectory(providerName, accountID)
	if err != nil {
		return "", CredentialSnapshot{}, err
	}
	lock, err := c.lockSlot(providerName, accountID)
	if err != nil {
		return "", CredentialSnapshot{}, err
	}
	defer func() { retErr = errors.Join(retErr, releaseSlot(lock)) }()
	consumed, err = c.readConsumedMarker(providerName, accountID)
	if err != nil || consumed == "" {
		return "", CredentialSnapshot{}, err
	}
	slot, err = c.readCredentialAt(providerName, accountDir, false)
	if IsCredentialMissing(err) {
		return "", CredentialSnapshot{}, nil
	}
	if err != nil {
		return "", CredentialSnapshot{}, fmt.Errorf("provideraccounts: read %s slot %s before promoting it: %w", providerName, accountID, err)
	}
	if credentialDigest(slot.Bytes()) == consumed {
		return "", CredentialSnapshot{}, nil
	}
	return consumed, slot, nil
}
