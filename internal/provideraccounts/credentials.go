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

// ErrSignedOutCredential reports a refusal to store the provider's own
// sign-out marker as if it were a credential. A husk is what the provider
// writes when it gives up on a token chain, so persisting one destroys
// whatever the destination held and leaves an account that looks logged in
// until the next request fails. The refusal lives in the write layer, not in
// caller discipline: every path that could reach a husk (a probe read-back, a
// rollback restore, a switch, an ephemeral seed) would otherwise need its own
// guard, and one forgotten guard is one unrecoverable login.
var ErrSignedOutCredential = errors.New("provideraccounts: credential is a provider sign-out, not a login")

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
	keychain claudeKeychain
	// signedOut reports that provider-authored credential bytes are the
	// provider's own post-refresh-failure sign-out marker (for Claude,
	// the blanked husk >= 2.1.219 writes on invalid_grant) rather than a
	// usable credential. Activation uses it to refuse to preserve such
	// bytes into an account slot, where they would overwrite the slot's
	// last saved pair. Nil means "never signed out"; the app wires the
	// provider-specific detector so this package stays provider-agnostic.
	signedOut func(providerName string, data []byte) bool
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
	return &Credentials{
		userHome: filepath.Clean(userHome),
		keychain: defaultClaudeKeychain(),
	}, nil
}

// NewCredentialsWithFileKeychain builds Credentials whose darwin Claude
// backend is the file-backed Keychain stand-in instead of security(1).
// The agent harness needs this: it boots the real app binary
// (testing.Testing() is false) against a redirected $HOME, which
// isolates every file store but NOT the Keychain — the active slot's
// service name ignores the home entirely, so a security(1)-backed
// harness run would read and write the developer's real Claude Code
// login. Harness runs use mock providers only, so no real credential
// is ever needed.
func NewCredentialsWithFileKeychain(userHome string) (*Credentials, error) {
	credentials, err := NewCredentials(userHome)
	if err != nil {
		return nil, err
	}
	credentials.keychain = fileClaudeKeychain{}
	return credentials, nil
}

// SetSignedOutDetector installs the provider-aware predicate for "these
// credential bytes are the provider's own sign-out marker, not a login".
func (c *Credentials) SetSignedOutDetector(detector func(providerName string, data []byte) bool) {
	c.signedOut = detector
}

func (c *Credentials) credentialSignedOut(providerName string, data []byte) bool {
	return c.signedOut != nil && c.signedOut(providerName, data)
}

// CredentialSignedOut reports that these bytes are the provider's own
// sign-out marker rather than a login, for callers that must answer the
// question before they have anywhere to write. No detector installed means
// no provider claims a sign-out shape, which answers false.
func (c *Credentials) CredentialSignedOut(providerName string, data []byte) bool {
	return c.credentialSignedOut(providerName, data)
}

func (c *Credentials) Paths(providerName string) (ProviderPaths, error) {
	switch providerName {
	case "claude":
		return ProviderPaths{
			SharedHome:     filepath.Join(c.userHome, ".claude"),
			CredentialFile: claudeCredentialFileName,
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
		return c.keychain.present(home, active)
	}
	info, statErr := os.Lstat(filepath.Join(home, paths.CredentialFile))
	if statErr != nil {
		return false, nil
	}
	return info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && info.Size() > 0, nil
}

// CredentialUsable reports whether this account could be activated right now:
// its credential is present AND is not the provider's sign-out husk. A husked
// slot and an empty one are the same state to the user — sign in again — so
// callers surfacing "needs login" must ask this rather than CredentialPresent,
// which only sees the file.
//
// One read answers both questions. Account listings ask this per account after
// every switch, refresh, and removal, and on macOS each credential access is a
// security(1) subprocess — a separate presence probe would double that for no
// extra information.
//
// A missing credential answers false with no error; a read that fails for any
// other reason propagates, so a caller can tell "definitely unusable" from
// "could not find out".
func (c *Credentials) CredentialUsable(providerName, accountID string, active bool) (bool, error) {
	snapshot, err := c.ReadCredentialSnapshot(providerName, accountID, active)
	if IsCredentialMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(snapshot.Data) == 0 {
		return false, nil
	}
	return !c.credentialSignedOut(providerName, snapshot.Data), nil
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
		data, err := c.keychain.read(home, active)
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

// writeCredentialAt is the ONE place opaque credential bytes become durable,
// and therefore the one place the sign-out refusal has to live.
func (c *Credentials) writeCredentialAt(
	providerName string,
	home string,
	active bool,
	data []byte,
) error {
	if c.credentialSignedOut(providerName, data) {
		return fmt.Errorf("%w (%s)", ErrSignedOutCredential, providerName)
	}
	return c.storeCredentialAt(providerName, home, active, data)
}

// storeCredentialAt performs the write itself. It is separate from
// writeCredentialAt for exactly one caller: the test helper that impersonates
// the provider CLI, which is the actor that legitimately writes a husk.
func (c *Credentials) storeCredentialAt(
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
		return c.keychain.write(home, active, data)
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
	if c.credentialSignedOut(providerName, data) {
		return fmt.Errorf("%w (%s)", ErrSignedOutCredential, providerName)
	}
	return c.storeActiveCredential(providerName, data)
}

// storeActiveCredential is writeActiveCredential without the sign-out refusal,
// for the provider-impersonating test helper only.
func (c *Credentials) storeActiveCredential(providerName string, data []byte) error {
	paths, err := c.Paths(providerName)
	if err != nil {
		return err
	}
	if runtime.GOOS != "darwin" || providerName != "claude" {
		activePath := filepath.Join(paths.SharedHome, paths.CredentialFile)
		if info, statErr := os.Lstat(activePath); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("provideraccounts: active credential path %s must not be a symlink", activePath)
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("provideraccounts: inspect active credential path: %w", statErr)
		}
	}
	return c.storeCredentialAt(providerName, paths.SharedHome, true, data)
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
