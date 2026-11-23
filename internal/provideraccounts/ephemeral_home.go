package provideraccounts

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/claudeconfig"
)

const maxConfigBytes = 16 << 20

// EphemeralHome is a short-lived isolated provider home used only for a new
// login or for probing a saved inactive account. It is never used by a normal
// chat session and must be cleaned after the provider process exits.
type EphemeralHome struct {
	Path         string
	providerName string
	credentials  *Credentials
	// lease is the registry lease held until cleanup succeeds; a failed
	// cleanup keeps it, so no sweep touches the home while this process
	// lives (ephemeral_registry.go).
	lease   *os.File
	cleaned bool
	// from and selected are what settleRotation needs to place the home's
	// rotation; landing is its answer, once Settle has run.
	from     ephemeralProvenance
	selected bool
	landing  *RotationLanding
}

// EphemeralOwner is the account a seeded temporary home probes.
type EphemeralOwner struct {
	AccountID string
	// Slot is the credential the owner's slot holds as the home is seeded:
	// the seed itself for an inactive account, and possibly an earlier one
	// for the selected account, whose seed is the canonical credential.
	Slot CredentialSnapshot
	// Selected reports that this store selected the owner, so a rotation
	// may replace the canonical credential (only the selecting store writes
	// it).
	Selected bool
}

// NewEphemeralHome creates an isolated provider home for a new login. Only the
// small provider configuration files required to preserve native login
// behavior are copied; session history and caches stay in the canonical home.
func (c *Credentials) NewEphemeralHome(providerName string) (*EphemeralHome, error) {
	return c.newEphemeralHome(providerName, nil, EphemeralOwner{})
}

// NewEphemeralHomeWithCredential creates an isolated provider home seeded with
// one caller-verified opaque credential snapshot, the owner's login: for an
// inactive account its slot, for the selected account the canonical
// credential recorded for it. The registry entry records what the seed and
// the owner's slot held, so a rotation is settled by the same rule live
// (Settle) and after a crash (SweepEphemeralCredentials).
func (c *Credentials) NewEphemeralHomeWithCredential(
	providerName string,
	credential []byte,
	owner EphemeralOwner,
) (*EphemeralHome, error) {
	if len(credential) == 0 {
		return nil, errors.New("provideraccounts: empty temporary credential snapshot")
	}
	// Refused before the home exists rather than at the seeding write: a
	// temporary home holding a sign-out is a CLI run that can only fail, and
	// its registry entry would then invite the boot sweep to reason about an
	// orphan that was never a login.
	if c.credentialSignedOut(providerName, credential) {
		return nil, fmt.Errorf("%w (%s)", ErrSignedOutCredential, providerName)
	}
	return c.newEphemeralHome(providerName, credential, owner)
}

func (c *Credentials) newEphemeralHome(
	providerName string,
	credential []byte,
	owner EphemeralOwner,
) (*EphemeralHome, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return nil, err
	}
	path, err := os.MkdirTemp("", ephemeralHomePrefix(providerName))
	if err != nil {
		return nil, fmt.Errorf("provideraccounts: create temporary %s home: %w", providerName, err)
	}
	from := ephemeralProvenance{AccountID: owner.AccountID}
	if credential != nil {
		from.SeedToken, _ = c.tokenFingerprint(providerName, credential)
		if !c.credentialSignedOut(providerName, owner.Slot.Bytes()) {
			from.SlotToken, _ = c.tokenFingerprint(providerName, owner.Slot.Bytes())
		}
	}
	home := &EphemeralHome{
		Path:         path,
		providerName: providerName,
		credentials:  c,
		from:         from,
		selected:     owner.Selected,
	}
	cleanupOnError := func(cause error) (*EphemeralHome, error) {
		if cleanupErr := home.Cleanup(); cleanupErr != nil {
			return nil, errors.Join(cause, cleanupErr)
		}
		return nil, cause
	}
	// Registration precedes everything that could place a credential in
	// the home — the CLI in a login home writes one on its own — because
	// an unrecorded home that crashes is invisible to the boot sweep
	// forever (ephemeral_registry.go).
	lease, err := c.registerEphemeralHome(providerName, path, from)
	if err != nil {
		// Never registered, so nothing outlives the directory.
		if removeErr := os.RemoveAll(path); removeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("provideraccounts: remove temporary %s home: %w", providerName, removeErr))
		}
		return nil, err
	}
	home.lease = lease
	if err := os.Chmod(path, 0o700); err != nil {
		return cleanupOnError(fmt.Errorf("provideraccounts: secure temporary %s home: %w", providerName, err))
	}
	for _, source := range ephemeralConfigSources(paths, providerName) {
		destination := filepath.Join(path, filepath.Base(source))
		if err := copyRegularFileIfPresent(source, destination, ephemeralConfigFilter(providerName, source)); err != nil {
			return cleanupOnError(err)
		}
	}
	if credential != nil {
		if err := c.writeCredentialAt(providerName, path, false, credential); err != nil {
			return cleanupOnError(fmt.Errorf("provideraccounts: seed temporary %s credentials: %w", providerName, err))
		}
	}
	return home, nil
}

func ephemeralConfigSources(paths ProviderPaths, providerName string) []string {
	switch providerName {
	case "claude":
		return []string{paths.GlobalConfig}
	case "codex":
		return []string{
			filepath.Join(paths.SharedHome, "config.toml"),
			filepath.Join(paths.SharedHome, "requirements.toml"),
		}
	case "agy":
		return []string{
			filepath.Join(paths.SharedHome, "settings.json"),
		}
	default:
		return nil
	}
}

// ephemeralConfigFilter rewrites one config file on its way into a
// temporary home. Only Claude needs it: `~/.claude.json` carries both
// the preferences a login flow needs and the canonical home's account
// identity. Copying the identity across would tell the CLI in the
// temporary home that it already knows who it is, so it would never
// derive the identity belonging to the credential it was actually
// seeded with — and a login there would be attributed to the canonical
// home's account. A nil filter copies bytes through unchanged.
func ephemeralConfigFilter(providerName, source string) func([]byte) ([]byte, error) {
	if providerName != "claude" || filepath.Base(source) != ".claude.json" {
		return nil
	}
	return claudeconfig.StripOAuthAccount
}

// copyRegularFileIfPresent copies source to destination when source
// exists, optionally passing the contents through filter first.
func copyRegularFileIfPresent(source, destination string, filter func([]byte) ([]byte, error)) error {
	info, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provideraccounts: inspect temporary-home config %s: %w", source, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("provideraccounts: temporary-home config %s is not a regular file", source)
	}
	if info.Size() > maxConfigBytes {
		return fmt.Errorf("provideraccounts: temporary-home config %s exceeds the size limit", source)
	}
	file, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("provideraccounts: open temporary-home config %s: %w", source, err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("provideraccounts: inspect opened temporary-home config %s: %w", source, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return fmt.Errorf("provideraccounts: temporary-home config %s changed while opening", source)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return fmt.Errorf("provideraccounts: read temporary-home config %s: %w", source, err)
	}
	if len(data) > maxConfigBytes {
		return fmt.Errorf("provideraccounts: temporary-home config %s exceeds the size limit", source)
	}
	currentInfo, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("provideraccounts: recheck temporary-home config %s: %w", source, err)
	}
	if !os.SameFile(openedInfo, currentInfo) {
		return fmt.Errorf("provideraccounts: temporary-home config %s changed while reading", source)
	}
	if filter != nil {
		data, err = filter(data)
		if err != nil {
			return fmt.Errorf("provideraccounts: filter temporary-home config %s: %w", source, err)
		}
	}
	if err := atomicfile.Write(destination, data); err != nil {
		return fmt.Errorf("provideraccounts: copy temporary-home config %s: %w", source, err)
	}
	return nil
}

func (c *Credentials) ReadEphemeralCredential(home *EphemeralHome) (CredentialSnapshot, error) {
	if home == nil || home.credentials != c || home.cleaned {
		return CredentialSnapshot{}, errors.New("provideraccounts: invalid temporary provider home")
	}
	return c.readCredentialAt(home.providerName, home.Path, false)
}

// Settle places the home's rotation, if it holds one, by the rule the boot
// sweep uses (settleRotation), and reports where it went. It runs once;
// Cleanup runs it too for a home nobody settled.
func (h *EphemeralHome) Settle() (RotationLanding, error) {
	if h == nil || h.cleaned {
		return RotationLanding{}, errors.New("provideraccounts: invalid temporary provider home")
	}
	if h.landing != nil {
		return *h.landing, nil
	}
	credential, err := h.credentials.readCredentialAt(h.providerName, h.Path, false)
	if IsCredentialMissing(err) {
		h.landing = &RotationLanding{}
		return *h.landing, nil
	}
	if err != nil {
		return RotationLanding{}, fmt.Errorf("provideraccounts: read the temporary %s home's credential: %w", h.providerName, err)
	}
	landing, err := h.credentials.settleRotation(h.providerName, h.from, credential.Bytes(), h.selected)
	if err != nil {
		return landing, err
	}
	h.landing = &landing
	return landing, nil
}

// Cleanup removes the home once its rotation is settled. A home whose
// rotation nothing took (RotationLanding.Kept), that could not be settled,
// or whose settle could not inspect every saved slot
// (RotationLanding.Unchecked) is kept with its entry for the boot sweep:
// its lease is released so the sweep can claim it, and the error says so.
func (h *EphemeralHome) Cleanup() error {
	if h == nil || h.cleaned {
		return nil
	}
	if h.lease != nil {
		landing, err := h.Settle()
		if err != nil || landing.Kept {
			return h.keepForSweep(errors.Join(ErrRotationKept, err, landing.Unchecked))
		}
		if landing.Unchecked != nil {
			return h.keepForSweep(fmt.Errorf("provideraccounts: the temporary %s home's rotation was saved, and the home is kept for the next start because a saved slot could not be checked for the token it spent: %w", h.providerName, landing.Unchecked))
		}
	}
	var cleanupErrs []error
	if kc, ok := h.credentials.keychainFor(h.providerName); ok {
		if err := kc.remove(h.Path, false); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	if err := os.RemoveAll(h.Path); err != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("provideraccounts: remove temporary %s home: %w", h.providerName, err))
	}
	// The registry entry goes last: it must outlive any partial cleanup,
	// or the boot sweep loses its only pointer to whatever remains.
	if len(cleanupErrs) > 0 {
		return errors.Join(cleanupErrs...)
	}
	removed, err := h.credentials.unregisterEphemeralHome(h.providerName, h.Path, h.lease)
	if removed {
		// The home and its entry are gone and the lease is spent; a failure
		// to end the lease cleanly is reported but leaves nothing to retry.
		h.cleaned = true
		h.lease = nil
	}
	return err
}

// ErrRotationKept reports that a temporary home holds a refreshed login no
// durable store took, and is kept for the next boot's sweep.
var ErrRotationKept = errors.New("provideraccounts: a refreshed login could not be saved and is kept for the next start")

// keepForSweep leaves the home and its entry, marked kept so the sweep
// reports it no second time, and releases the lease so the sweep may claim
// it. cause says why it is kept.
func (h *EphemeralHome) keepForSweep(cause error) error {
	errs := []error{cause}
	dir, err := h.credentials.ephemeralRegistryDir(h.providerName)
	if err == nil {
		entryPath := filepath.Join(dir, ephemeralRegistryFileName(h.Path))
		var entry ephemeralRegistryEntry
		if found, readErr := atomicfile.ReadJSON(entryPath, &entry); readErr != nil || !found {
			err = errors.Join(readErr, errors.New("provideraccounts: the kept home's registry entry is gone"))
		} else {
			entry.Kept = true
			err = atomicfile.WriteJSON(entryPath, entry)
		}
	}
	errs = append(errs, err, releaseLease(h.lease))
	h.cleaned = true
	h.lease = nil
	return errors.Join(errs...)
}
