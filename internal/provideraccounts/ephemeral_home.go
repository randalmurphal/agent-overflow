package provideraccounts

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

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
	cleaned      bool
}

// NewEphemeralHome creates an isolated provider home for a new login. Only the
// small provider configuration files required to preserve native login
// behavior are copied; session history and caches stay in the canonical home.
func (c *Credentials) NewEphemeralHome(providerName string) (*EphemeralHome, error) {
	return c.newEphemeralHome(providerName, nil)
}

// NewEphemeralHomeWithCredential creates an isolated provider home seeded with
// one caller-verified opaque credential snapshot. It is used for inactive
// account probes without rereading mutable account storage after the caller
// releases its selection lock.
func (c *Credentials) NewEphemeralHomeWithCredential(
	providerName string,
	credential []byte,
) (*EphemeralHome, error) {
	if len(credential) == 0 {
		return nil, errors.New("provideraccounts: empty temporary credential snapshot")
	}
	return c.newEphemeralHome(providerName, credential)
}

func (c *Credentials) newEphemeralHome(
	providerName string,
	credential []byte,
) (*EphemeralHome, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return nil, err
	}
	path, err := os.MkdirTemp("", "agent-overflow-"+providerName+"-")
	if err != nil {
		return nil, fmt.Errorf("provideraccounts: create temporary %s home: %w", providerName, err)
	}
	home := &EphemeralHome{
		Path:         path,
		providerName: providerName,
		credentials:  c,
	}
	cleanupOnError := func(cause error) (*EphemeralHome, error) {
		if cleanupErr := home.Cleanup(); cleanupErr != nil {
			return nil, errors.Join(cause, cleanupErr)
		}
		return nil, cause
	}
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

func (h *EphemeralHome) Cleanup() error {
	if h == nil || h.cleaned {
		return nil
	}
	var cleanupErrs []error
	if runtime.GOOS == "darwin" && h.providerName == "claude" {
		if err := deleteClaudeKeychainCredential(h.Path, false); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	if err := os.RemoveAll(h.Path); err != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("provideraccounts: remove temporary %s home: %w", h.providerName, err))
	}
	err := errors.Join(cleanupErrs...)
	if err == nil {
		h.cleaned = true
	}
	return err
}
