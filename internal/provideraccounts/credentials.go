package provideraccounts

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"agent-overflow/internal/atomicfile"
)

const (
	profileDirectoryName = "agent-overflow-accounts"
	maxCredentialBytes   = 16 << 20
)

var safeAccountID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type ProviderPaths struct {
	SharedHome     string
	CredentialFile string
	GlobalConfig   string
}

// Credentials manages provider-native credential files below an injected user
// home. It never decodes their contents.
type Credentials struct {
	userHome string
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

func (c *Credentials) ProfileHome(providerName, accountID string) (string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return "", err
	}
	if !safeAccountID.MatchString(accountID) {
		return "", fmt.Errorf("provideraccounts: invalid account id %q", accountID)
	}
	return filepath.Join(paths.SharedHome, profileDirectoryName, accountID), nil
}

func (c *Credentials) ProfileCredentialPath(providerName, accountID string) (string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return "", err
	}
	home, err := c.ProfileHome(providerName, accountID)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, paths.CredentialFile), nil
}

// RemoveProfile deletes a transient or superseded login home. The account ID
// validation in ProfileHome confines removal to the provider-managed profile
// root; symlinked shared entries are unlinked, never traversed.
func (c *Credentials) RemoveProfile(providerName, accountID string) error {
	profileHome, err := c.ProfileHome(providerName, accountID)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" && providerName == "claude" {
		if err := deleteClaudeKeychainCredential(profileHome, false); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(profileHome); err != nil {
		return fmt.Errorf("provideraccounts: remove login profile: %w", err)
	}
	return nil
}

// PrepareLoginHome creates an isolated provider home whose credential file is
// private while every existing non-secret top-level entry points at the shared
// native provider home.
func (c *Credentials) PrepareLoginHome(providerName, accountID string) (string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return "", err
	}
	profileHome, err := c.ProfileHome(providerName, accountID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(paths.SharedHome, 0o700); err != nil {
		return "", fmt.Errorf("provideraccounts: create %s home: %w", providerName, err)
	}
	if err := os.Chmod(paths.SharedHome, 0o700); err != nil {
		return "", fmt.Errorf("provideraccounts: secure %s home: %w", providerName, err)
	}
	profileRoot := filepath.Dir(profileHome)
	if err := os.MkdirAll(profileRoot, 0o700); err != nil {
		return "", fmt.Errorf("provideraccounts: create profile root: %w", err)
	}
	if err := os.Chmod(profileRoot, 0o700); err != nil {
		return "", fmt.Errorf("provideraccounts: secure profile root: %w", err)
	}
	profileInfo, err := os.Lstat(profileHome)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(profileHome, 0o700); err != nil {
			return "", fmt.Errorf("provideraccounts: create login home: %w", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("provideraccounts: inspect login home: %w", err)
	} else if profileInfo.Mode()&os.ModeSymlink != 0 || !profileInfo.IsDir() {
		return "", fmt.Errorf("provideraccounts: login home %s is not a managed directory", profileHome)
	}
	if err := os.Chmod(profileHome, 0o700); err != nil {
		return "", fmt.Errorf("provideraccounts: secure login home: %w", err)
	}

	entries, err := os.ReadDir(paths.SharedHome)
	if err != nil {
		return "", fmt.Errorf("provideraccounts: list shared %s home: %w", providerName, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == paths.CredentialFile || name == profileDirectoryName {
			continue
		}
		if err := ensureSharedSymlink(
			filepath.Join(profileHome, name),
			filepath.Join(paths.SharedHome, name),
		); err != nil {
			return "", err
		}
	}

	// CLAUDE_CONFIG_DIR relocates ~/.claude.json into the selected config
	// directory. Link that global config explicitly so account isolation does
	// not duplicate preferences, projects, MCP state, or session history.
	if providerName == "claude" {
		if _, statErr := os.Lstat(paths.GlobalConfig); statErr == nil {
			if err := ensureSharedSymlink(filepath.Join(profileHome, ".claude.json"), paths.GlobalConfig); err != nil {
				return "", err
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("provideraccounts: inspect Claude global config: %w", statErr)
		}
	}
	return profileHome, nil
}

// ReconcileProfile moves any non-credential top-level state created inside
// an isolated provider home back into the canonical native home, then replaces
// it with a symlink. Providers occasionally add a new cache or history path
// between releases; this keeps those additions shared without teaching Agent
// Overflow their names or inspecting their contents.
func (c *Credentials) ReconcileProfile(providerName, accountID string) error {
	paths, err := c.Paths(providerName)
	if err != nil {
		return err
	}
	profileHome, err := c.ProfileHome(providerName, accountID)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(profileHome)
	if err != nil {
		return fmt.Errorf("provideraccounts: list %s profile: %w", providerName, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == paths.CredentialFile {
			continue
		}
		profilePath := filepath.Join(profileHome, name)
		targetPath := filepath.Join(paths.SharedHome, name)
		if providerName == "claude" && name == ".claude.json" {
			targetPath = paths.GlobalConfig
		}

		info, err := os.Lstat(profilePath)
		if err != nil {
			return fmt.Errorf("provideraccounts: inspect profile entry %s: %w", profilePath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := ensureSharedSymlink(profilePath, targetPath); err != nil {
				return err
			}
			continue
		}
		if _, err := os.Lstat(targetPath); err == nil {
			return fmt.Errorf(
				"provideraccounts: cannot reconcile private profile entry %s because shared state already exists",
				profilePath,
			)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("provideraccounts: inspect shared entry %s: %w", targetPath, err)
		}
		if err := os.Rename(profilePath, targetPath); err != nil {
			return fmt.Errorf("provideraccounts: move profile entry %s into shared state: %w", profilePath, err)
		}
		if err := ensureSharedSymlink(profilePath, targetPath); err != nil {
			return err
		}
	}
	return nil
}

func ensureSharedSymlink(linkPath, targetPath string) error {
	info, err := os.Lstat(linkPath)
	if err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("provideraccounts: shared login-home entry %s is not a symlink", linkPath)
		}
		target, readErr := os.Readlink(linkPath)
		if readErr != nil {
			return fmt.Errorf("provideraccounts: read symlink %s: %w", linkPath, readErr)
		}
		if filepath.Clean(target) != filepath.Clean(targetPath) {
			return fmt.Errorf("provideraccounts: shared login-home entry %s points to unexpected target", linkPath)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("provideraccounts: inspect login-home entry %s: %w", linkPath, err)
	}
	if err := os.Symlink(targetPath, linkPath); err != nil {
		return fmt.Errorf("provideraccounts: link shared entry %s: %w", linkPath, err)
	}
	return nil
}

// ImportActive snapshots the provider's current native credential file into a
// profile. It is used once when adopting an already-authenticated account and
// before every switch away so provider refresh-token rotations are retained.
func (c *Credentials) ImportActive(providerName, accountID string) error {
	data, err := c.ReadCredential(providerName, "", true)
	if err != nil {
		return err
	}
	if err := c.writeCredential(providerName, accountID, false, data); err != nil {
		return fmt.Errorf("provideraccounts: snapshot active %s credentials: %w", providerName, err)
	}
	return nil
}

// ReadCredential returns opaque bytes from the provider's native credential
// store. On macOS, Claude owns a config-home-scoped Keychain entry; on other
// platforms (and for Codex) the native store is the provider credential file.
func (c *Credentials) ReadCredential(providerName, accountID string, active bool) ([]byte, error) {
	home, path, err := c.credentialLocation(providerName, accountID, active)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "darwin" && providerName == "claude" {
		return readClaudeKeychainCredential(home, active)
	}
	return readCredential(path)
}

func (c *Credentials) credentialLocation(providerName, accountID string, active bool) (string, string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return "", "", err
	}
	home := paths.SharedHome
	if !active {
		home, err = c.ProfileHome(providerName, accountID)
		if err != nil {
			return "", "", err
		}
	}
	return home, filepath.Join(home, paths.CredentialFile), nil
}

func (c *Credentials) writeCredential(providerName, accountID string, active bool, data []byte) error {
	home, path, err := c.credentialLocation(providerName, accountID, active)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" && providerName == "claude" {
		return writeClaudeKeychainCredential(home, active, data)
	}
	return atomicfile.Write(path, data)
}

func (c *Credentials) CopyProfile(providerName, sourceAccountID, targetAccountID string) error {
	data, err := c.ReadCredential(providerName, sourceAccountID, false)
	if err != nil {
		return fmt.Errorf("provideraccounts: read login profile: %w", err)
	}
	if err := c.writeCredential(providerName, targetAccountID, false, data); err != nil {
		return fmt.Errorf("provideraccounts: update account profile: %w", err)
	}
	return nil
}

// Activate atomically copies targetAccountID's provider-native credential
// bytes into the provider's active home. currentAccountID may be empty; when
// present, the current bytes are first snapshotted back into that profile.
func (c *Credentials) Activate(providerName, currentAccountID, targetAccountID string) error {
	if currentAccountID != "" && currentAccountID != targetAccountID {
		if err := c.preserveCurrent(providerName, currentAccountID); err != nil {
			return fmt.Errorf("provideraccounts: preserve current account before switch: %w", err)
		}
	}
	data, err := c.ReadCredential(providerName, targetAccountID, false)
	if err != nil {
		return fmt.Errorf("provideraccounts: read selected credentials: %w", err)
	}
	if runtime.GOOS == "darwin" && providerName == "claude" {
		if err := c.writeCredential(providerName, "", true, data); err != nil {
			return fmt.Errorf("provideraccounts: activate %s credentials: %w", providerName, err)
		}
		return nil
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
	if err := atomicfile.Write(activePath, data); err != nil {
		return fmt.Errorf("provideraccounts: activate %s credentials: %w", providerName, err)
	}
	return nil
}

func (c *Credentials) preserveCurrent(providerName, accountID string) error {
	if providerName != "codex" {
		return c.ImportActive(providerName, accountID)
	}
	activePath, err := c.ActiveCredentialPath(providerName)
	if err != nil {
		return err
	}
	profilePath, err := c.ProfileCredentialPath(providerName, accountID)
	if err != nil {
		return err
	}
	activeInfo, err := os.Lstat(activePath)
	if err != nil {
		return err
	}
	if activeInfo.Mode()&os.ModeSymlink != 0 || !activeInfo.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular credential file", activePath)
	}
	profileInfo, profileErr := os.Lstat(profilePath)
	if errors.Is(profileErr, os.ErrNotExist) {
		return c.ImportActive(providerName, accountID)
	}
	if profileErr != nil {
		return profileErr
	}
	if profileInfo.Mode()&os.ModeSymlink != 0 || !profileInfo.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular credential file", profilePath)
	}
	// Managed Codex app-servers refresh the isolated profile; external Codex
	// processes refresh the canonical native file. Preserve whichever side
	// was written most recently instead of overwriting a rotated refresh token
	// with the older copy.
	if activeInfo.ModTime().After(profileInfo.ModTime()) {
		return c.ImportActive(providerName, accountID)
	}
	return nil
}

func (c *Credentials) RemoveActive(providerName string) error {
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

func readCredential(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular credential file", path)
	}
	if info.Size() > maxCredentialBytes {
		return nil, fmt.Errorf("%s exceeds the credential size limit", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return data, nil
}

func IsCredentialMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrCredentialMissing)
}
