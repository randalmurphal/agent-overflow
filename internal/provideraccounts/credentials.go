package provideraccounts

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"agent-overflow/internal/atomicfile"
)

const (
	accountDirectoryName = "agent-overflow-accounts"
	maxCredentialBytes   = 16 << 20
)

var safeAccountID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type ProviderPaths struct {
	SharedHome     string
	CredentialFile string
	GlobalConfig   string
}

// Credentials owns Agent Overflow's opaque copies of provider-native
// credentials. Provider processes always run against SharedHome; saved account
// directories receive only the provider's credential file from Agent Overflow
// (or, on macOS for Claude, identify a config-scoped native Keychain entry).
// Unrecognized contents in registered directories are ignored.
type Credentials struct {
	userHome string
}

// CredentialSnapshot is one stable read from a provider-native credential
// store. Data remains opaque to Agent Overflow.
type CredentialSnapshot struct {
	Data []byte
}

func NewCredentials(userHome string) (*Credentials, error) {
	if strings.TrimSpace(userHome) == "" {
		return nil, errors.New("provideraccounts: empty user home")
	}
	return &Credentials{userHome: filepath.Clean(userHome)}, nil
}

func (c *Credentials) Paths(providerName string) (ProviderPaths, error) {
	switch providerName {
	case "claude":
		return ProviderPaths{
			SharedHome:     filepath.Join(c.userHome, ".claude"),
			CredentialFile: ".credentials.json",
			GlobalConfig:   filepath.Join(c.userHome, ".claude.json"),
		}, nil
	case "codex":
		return ProviderPaths{
			SharedHome:     filepath.Join(c.userHome, ".codex"),
			CredentialFile: "auth.json",
		}, nil
	default:
		return ProviderPaths{}, fmt.Errorf("provideraccounts: unsupported provider %q", providerName)
	}
}

func (c *Credentials) ActiveCredentialPath(providerName string) (string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return "", err
	}
	return filepath.Join(paths.SharedHome, paths.CredentialFile), nil
}

func (c *Credentials) AccountCredentialPath(providerName, accountID string) (string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return "", err
	}
	accountDir, err := c.accountDirectory(providerName, accountID)
	if err != nil {
		return "", err
	}
	return filepath.Join(accountDir, paths.CredentialFile), nil
}

func (c *Credentials) accountDirectory(providerName, accountID string) (string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return "", err
	}
	if !safeAccountID.MatchString(accountID) {
		return "", fmt.Errorf("provideraccounts: invalid account id %q", accountID)
	}
	root := filepath.Join(paths.SharedHome, accountDirectoryName)
	if err := validateDirectoryIfPresent(root, "managed account root"); err != nil {
		return "", err
	}
	accountDir := filepath.Join(root, accountID)
	if err := validateDirectoryIfPresent(accountDir, "managed account directory"); err != nil {
		return "", err
	}
	return accountDir, nil
}

func validateDirectoryIfPresent(path, label string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provideraccounts: inspect %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("provideraccounts: %s %s is not a directory", label, path)
	}
	return nil
}

func (c *Credentials) ensureAccountDirectory(providerName, accountID string) (string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return "", err
	}
	accountDir, err := c.accountDirectory(providerName, accountID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(paths.SharedHome, 0o700); err != nil {
		return "", fmt.Errorf("provideraccounts: create %s home: %w", providerName, err)
	}
	if err := os.Chmod(paths.SharedHome, 0o700); err != nil {
		return "", fmt.Errorf("provideraccounts: secure %s home: %w", providerName, err)
	}
	root := filepath.Dir(accountDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("provideraccounts: create managed account root: %w", err)
	}
	if err := validateDirectoryIfPresent(root, "managed account root"); err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("provideraccounts: secure managed account root: %w", err)
	}
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		return "", fmt.Errorf("provideraccounts: create managed account directory: %w", err)
	}
	if err := validateDirectoryIfPresent(accountDir, "managed account directory"); err != nil {
		return "", err
	}
	if err := os.Chmod(accountDir, 0o700); err != nil {
		return "", fmt.Errorf("provideraccounts: secure managed account directory: %w", err)
	}
	return accountDir, nil
}

// CredentialPresent reports whether the credential this account would be
// activated from is readable: the canonical native store when active is
// true, that account's saved slot otherwise.
//
// Metadata can outlive its credential — an interrupted registration, a
// slot lost to an older storage layout, a file removed by hand. Such an
// account can never be selected, so callers surface it as needing a
// fresh login rather than letting a switch fail with a raw filesystem
// error. Unreadable counts as absent: for this question a corrupt or
// non-regular credential is no more usable than a missing one.
func (c *Credentials) CredentialPresent(providerName, accountID string, active bool) (bool, error) {
	home, err := c.credentialLocation(providerName, accountID, active)
	if err != nil {
		return false, err
	}
	paths, err := c.Paths(providerName)
	if err != nil {
		return false, err
	}
	if runtime.GOOS == "darwin" && providerName == "claude" {
		// The Keychain has no cheap existence check; the read is the probe.
		_, readErr := readClaudeKeychainCredential(home, active)
		return readErr == nil, nil
	}
	info, statErr := os.Lstat(filepath.Join(home, paths.CredentialFile))
	if statErr != nil {
		return false, nil
	}
	return info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && info.Size() > 0, nil
}

// ReadCredential returns opaque bytes from the canonical native store when
// active is true, or from one saved account slot otherwise.
func (c *Credentials) ReadCredential(providerName, accountID string, active bool) ([]byte, error) {
	snapshot, err := c.ReadCredentialSnapshot(providerName, accountID, active)
	return snapshot.Data, err
}

func (c *Credentials) ReadCredentialSnapshot(
	providerName string,
	accountID string,
	active bool,
) (CredentialSnapshot, error) {
	home, err := c.credentialLocation(providerName, accountID, active)
	if err != nil {
		return CredentialSnapshot{}, err
	}
	return c.readCredentialAt(providerName, home, active)
}

func (c *Credentials) readCredentialAt(
	providerName string,
	home string,
	active bool,
) (CredentialSnapshot, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return CredentialSnapshot{}, err
	}
	if runtime.GOOS == "darwin" && providerName == "claude" {
		data, err := readClaudeKeychainCredential(home, active)
		return CredentialSnapshot{Data: data}, err
	}
	return readCredentialSnapshot(filepath.Join(home, paths.CredentialFile))
}

func (c *Credentials) credentialLocation(
	providerName string,
	accountID string,
	active bool,
) (string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return "", err
	}
	home := paths.SharedHome
	if !active {
		home, err = c.accountDirectory(providerName, accountID)
		if err != nil {
			return "", err
		}
	}
	return home, nil
}

func (c *Credentials) writeCredentialAt(
	providerName string,
	home string,
	active bool,
	data []byte,
) error {
	if len(data) == 0 {
		return errors.New("provideraccounts: empty credential snapshot")
	}
	if len(data) > maxCredentialBytes {
		return errors.New("provideraccounts: credential snapshot exceeds the size limit")
	}
	paths, err := c.Paths(providerName)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" && providerName == "claude" {
		return writeClaudeKeychainCredential(home, active, data)
	}
	return atomicfile.Write(filepath.Join(home, paths.CredentialFile), data)
}

func (c *Credentials) writeCredential(
	providerName string,
	accountID string,
	active bool,
	data []byte,
) error {
	home, err := c.credentialLocation(providerName, accountID, active)
	if err != nil {
		return err
	}
	if !active {
		home, err = c.ensureAccountDirectory(providerName, accountID)
		if err != nil {
			return err
		}
	}
	return c.writeCredentialAt(providerName, home, active, data)
}

func (c *Credentials) WriteAccountCredential(providerName, accountID string, data []byte) error {
	return c.writeCredential(providerName, accountID, false, data)
}

// CommitSelectedCredential publishes a refreshed credential to the canonical
// native store before updating its saved account slot. Canonical-first ordering
// keeps the active account usable if the secondary slot write fails; a later
// reconciliation can repair the slot from canonical state.
func (c *Credentials) CommitSelectedCredential(
	providerName string,
	accountID string,
	data []byte,
) error {
	if err := c.writeActiveCredential(providerName, data); err != nil {
		return fmt.Errorf("provideraccounts: activate refreshed %s credentials: %w", providerName, err)
	}
	if err := c.WriteAccountCredential(providerName, accountID, data); err != nil {
		return fmt.Errorf(
			"provideraccounts: save refreshed %s account after canonical activation: %w",
			providerName,
			err,
		)
	}
	return nil
}

func (c *Credentials) writeActiveCredential(providerName string, data []byte) error {
	if runtime.GOOS == "darwin" && providerName == "claude" {
		return c.writeCredential(providerName, "", true, data)
	}
	activePath, err := c.ActiveCredentialPath(providerName)
	if err != nil {
		return err
	}
	if info, statErr := os.Lstat(activePath); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("provideraccounts: active credential path %s must not be a symlink", activePath)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("provideraccounts: inspect active credential path: %w", statErr)
	}
	return atomicfile.Write(activePath, data)
}

// Activate atomically replaces the canonical provider credential with the
// selected account. When switching away from currentAccountID, the current
// canonical bytes are first preserved in that account slot so native refresh
// token rotations are not lost.
func (c *Credentials) Activate(providerName, currentAccountID, targetAccountID string) error {
	var current *CredentialSnapshot
	if currentAccountID != "" && currentAccountID != targetAccountID {
		snapshot, err := c.ReadCredentialSnapshot(providerName, "", true)
		if err != nil {
			return fmt.Errorf("provideraccounts: read current account before switch: %w", err)
		}
		current = &snapshot
	}
	return c.ActivateWithSnapshot(providerName, currentAccountID, targetAccountID, current)
}

// ActivateWithSnapshot is Activate with a caller-verified current credential.
// It prevents an external login racing activation from being saved under the
// stale current account ID.
func (c *Credentials) ActivateWithSnapshot(
	providerName string,
	currentAccountID string,
	targetAccountID string,
	current *CredentialSnapshot,
) error {
	if currentAccountID != "" && currentAccountID != targetAccountID {
		if current == nil {
			return errors.New("provideraccounts: current credential snapshot is required")
		}
		if err := c.WriteAccountCredential(providerName, currentAccountID, current.Data); err != nil {
			return fmt.Errorf("provideraccounts: preserve current account before switch: %w", err)
		}
	}
	data, err := c.ReadCredential(providerName, targetAccountID, false)
	if err != nil {
		return fmt.Errorf("provideraccounts: read selected credentials: %w", err)
	}
	// Retire the outgoing identity before the incoming credential lands.
	// Ordered this way every outcome converges on a consistent pair: if
	// the clear fails nothing has moved, and if the credential write
	// fails the provider re-derives the identity it already had. The
	// reverse order has a failure mode that does not self-heal — new
	// tokens described by the previous account's identity.
	if currentAccountID != targetAccountID {
		if err := c.retireProviderIdentity(providerName); err != nil {
			return err
		}
	}
	if err := c.writeActiveCredential(providerName, data); err != nil {
		return fmt.Errorf("provideraccounts: activate %s credentials: %w", providerName, err)
	}
	return nil
}

// RemoveActive signs the provider out of its canonical home. The
// identity record goes with the credential — leaving it behind would
// describe a login that no longer exists, which is the same split-state
// bug a switch avoids and exactly what the provider's own logout
// clears.
func (c *Credentials) RemoveActive(providerName string) error {
	if err := c.retireProviderIdentity(providerName); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" && providerName == "claude" {
		paths, err := c.Paths(providerName)
		if err != nil {
			return err
		}
		return deleteClaudeKeychainCredential(paths.SharedHome, true)
	}
	activePath, err := c.ActiveCredentialPath(providerName)
	if err != nil {
		return err
	}
	info, err := os.Lstat(activePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provideraccounts: inspect active credentials: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("provideraccounts: active credential path %s is not a regular file", activePath)
	}
	if err := os.Remove(activePath); err != nil {
		return fmt.Errorf("provideraccounts: remove active credentials: %w", err)
	}
	return nil
}

// RemoveAccount deletes one saved credential slot. The validated account ID
// and non-symlink managed root confine removal to Agent Overflow's own storage.
func (c *Credentials) RemoveAccount(providerName, accountID string) error {
	paths, err := c.Paths(providerName)
	if err != nil {
		return err
	}
	accountDir, err := c.accountDirectory(providerName, accountID)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" && providerName == "claude" {
		if err := deleteClaudeKeychainCredential(accountDir, false); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(paths.SharedHome)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provideraccounts: open %s home for account removal: %w", providerName, err)
	}
	defer root.Close()
	if err := root.RemoveAll(filepath.Join(accountDirectoryName, accountID)); err != nil {
		return fmt.Errorf("provideraccounts: remove saved account credentials: %w", err)
	}
	return nil
}

// PruneOrphanedAccounts removes credential slots that have no corresponding
// metadata account and returns the IDs it removed, so the caller can record
// each destruction. A crash can leave one slot behind between credential
// creation and metadata commit; registered account directories are never
// inspected or modified by this sweep.
//
// An empty keep-set prunes nothing. Zero registered accounts cannot be told
// apart from a process reading a different metadata store than the one these
// slots belong to — a fresh --data-dir, a test overriding the config root
// but not the home — and the slots hold logins whose refresh tokens are
// single-use and unrecoverable, so the sweep refuses to guess. The crash
// orphan it exists for is cleaned on the first sweep after any account is
// registered again.
func (c *Credentials) PruneOrphanedAccounts(
	providerName string,
	keepAccountIDs map[string]bool,
) ([]string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return nil, err
	}
	rootPath := filepath.Join(paths.SharedHome, accountDirectoryName)
	info, err := os.Lstat(rootPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("provideraccounts: inspect managed account root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("provideraccounts: managed account root %s is not a directory", rootPath)
	}
	if len(keepAccountIDs) == 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		return nil, fmt.Errorf("provideraccounts: list managed accounts: %w", err)
	}
	var pruned []string
	var pruneErrs []error
	for _, entry := range entries {
		accountID := entry.Name()
		if !safeAccountID.MatchString(accountID) {
			pruneErrs = append(pruneErrs, fmt.Errorf(
				"provideraccounts: invalid managed account entry %q",
				accountID,
			))
			continue
		}
		if keepAccountIDs[accountID] {
			continue
		}
		if err := c.RemoveAccount(providerName, accountID); err != nil {
			pruneErrs = append(pruneErrs, err)
			continue
		}
		pruned = append(pruned, accountID)
	}
	return pruned, errors.Join(pruneErrs...)
}

func readCredentialSnapshot(path string) (CredentialSnapshot, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return CredentialSnapshot{}, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return CredentialSnapshot{}, fmt.Errorf("%s is not a regular credential file", path)
	}
	if pathInfo.Size() > maxCredentialBytes {
		return CredentialSnapshot{}, fmt.Errorf("%s exceeds the credential size limit", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return CredentialSnapshot{}, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return CredentialSnapshot{}, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return CredentialSnapshot{}, fmt.Errorf("%s changed while opening credential file", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if err != nil {
		return CredentialSnapshot{}, err
	}
	if len(data) > maxCredentialBytes {
		return CredentialSnapshot{}, fmt.Errorf("%s exceeds the credential size limit", path)
	}
	if len(data) == 0 {
		return CredentialSnapshot{}, fmt.Errorf("%s is empty", path)
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return CredentialSnapshot{}, err
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, currentInfo) {
		return CredentialSnapshot{}, fmt.Errorf("%s changed while reading credential file", path)
	}
	return CredentialSnapshot{Data: data}, nil
}

func IsCredentialMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrCredentialMissing)
}
