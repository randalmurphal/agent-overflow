package provideraccounts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/atomicfile"
)

var ErrCredentialMissing = errors.New("provideraccounts: native credential not found")

// claudeCredentialFileName is Claude Code's credential filename, shared
// by Paths and the file-backed Keychain stand-in so the two layouts
// cannot drift.
const claudeCredentialFileName = ".credentials.json"

// claudeKeychain is the seam between Credentials and the macOS Keychain.
//
// It exists for test isolation, not for polymorphism. The active slot's
// Keychain service name is FIXED ("Claude Code-credentials" — Claude
// Code's own login item) no matter which userHome a Credentials was
// built with, so there is no such thing as temp-dir isolation for the
// real Keychain: a test or harness run that injects a temporary home
// still targets the developer's real login keychain through this
// boundary. Incident 2026-08-01: before the seam, `make go-test` on a
// dev Mac overwrote the developer's real Claude Code login with fixture
// bytes (forcing a re-login) and littered the login keychain with
// hash-scoped test items.
//
// NewCredentials therefore installs fileClaudeKeychain whenever the
// process is a test binary (testing.Testing()); the harness boot
// selects it explicitly via NewCredentialsWithFileKeychain. Never
// construct securityClaudeKeychain in test code, and never add a code
// path that shells out to security(1) without going through this
// interface (pinned by TestNoSecurityCallsOutsideTheKeychainSeam).
type claudeKeychain interface {
	read(configHome string, active bool) ([]byte, error)
	write(configHome string, active bool, data []byte) error
	remove(configHome string, active bool) error
	// present answers "could this credential be read" without pulling
	// the secret bytes: account listings probe every slot, and on the
	// real Keychain a full read is a subprocess spawn returning the
	// password data.
	present(configHome string, active bool) (bool, error)
}

// defaultClaudeKeychain picks the backend for a new Credentials: the
// real security(1) Keychain in normal runs, the file-backed stand-in
// inside `go test`.
func defaultClaudeKeychain() claudeKeychain {
	if testing.Testing() {
		return fileClaudeKeychain{}
	}
	return securityClaudeKeychain{}
}

// validateClaudeKeychainWrite is the one write-size rule for every
// Keychain backend. Shared so the stand-in cannot drift from the
// security(1) backend it mirrors.
func validateClaudeKeychainWrite(data []byte) error {
	if len(data) == 0 || len(data) > maxCredentialBytes {
		return errors.New("provideraccounts: invalid Claude Keychain credential size")
	}
	return nil
}

// securityInteractiveLineLimit is the largest `security -i` command we
// hand to the interactive reader. security(1) reads interactive input
// into a fixed 4096-byte line buffer; a longer line is SPLIT, executing
// the head with truncated arguments and the tail as a second command —
// for add-generic-password -U that would overwrite a good credential
// with a truncated one. 4032 (Claude Code's own threshold) keeps a
// safety margin under 4096.
const securityInteractiveLineLimit = 4032

// securityClaudeKeychain is the production backend: the user's login
// keychain via the security(1) CLI, using Claude Code's native service
// naming so the active credential IS Claude Code's own login item.
type securityClaudeKeychain struct{}

func (securityClaudeKeychain) read(configHome string, active bool) ([]byte, error) {
	service, username, err := claudeKeychainIdentity(configHome, active)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("security", "find-generic-password", "-a", username, "-w", "-s", service)
	output, err := cmd.Output()
	if err != nil {
		return nil, ErrCredentialMissing
	}
	// security -w appends a trailing newline to the stored value.
	data := bytes.TrimSpace(output)
	if len(data) == 0 {
		return nil, ErrCredentialMissing
	}
	if len(data) > maxCredentialBytes {
		return nil, errors.New("provideraccounts: Claude Keychain credential exceeds size limit")
	}
	return data, nil
}

func (securityClaudeKeychain) present(configHome string, active bool) (bool, error) {
	service, username, err := claudeKeychainIdentity(configHome, active)
	if err != nil {
		return false, err
	}
	// No -w: an attribute-only lookup never returns the secret, so the
	// probe costs a subprocess but not a credential read. Only exit 44
	// ("no such item") is a real answer of "absent". Every other failure
	// — locked keychain, a spawn failure under load — is an error, NOT
	// absence: account listings map a failed presence check to "still
	// usable", and collapsing errors into false here painted saved
	// accounts with a spurious "Sign in again" while the credential sat
	// intact in the Keychain.
	cmd := exec.Command("security", "find-generic-password", "-a", username, "-s", service)
	cmd.Stdout = nil
	cmd.Stderr = nil
	err = cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
		return false, nil
	}
	return false, fmt.Errorf("provideraccounts: query Claude Keychain credential: %v", err)
}

func (securityClaudeKeychain) write(configHome string, active bool, data []byte) error {
	if err := validateClaudeKeychainWrite(data); err != nil {
		return err
	}
	service, username, err := claudeKeychainIdentity(configHome, active)
	if err != nil {
		return err
	}
	hexValue := hex.EncodeToString(data)
	command := fmt.Sprintf(
		"add-generic-password -U -a %s -s %s -X %s\n",
		strconv.Quote(username),
		strconv.Quote(service),
		strconv.Quote(hexValue),
	)
	var cmd *exec.Cmd
	if len(command) <= securityInteractiveLineLimit {
		cmd = exec.Command("security", "-i")
		cmd.Stdin = strings.NewReader(command)
	} else {
		// Matches Claude Code's native fallback for Keychain payloads
		// larger than security(1)'s interactive line buffer. The value
		// is hex-encoded and provider output is never surfaced — but it
		// rides in argv, which any same-uid process can read via
		// KERN_PROCARGS2 for the duration of the call, unlike the
		// ACL-gated item it lands in. Real Claude credentials (~1-2KB)
		// stay on the -i path; see the review note in AGENTS.md before
		// relying on this branch for anything bigger.
		cmd = exec.Command(
			"security", "add-generic-password", "-U",
			"-a", username, "-s", service, "-X", hexValue,
		)
	}
	if err := cmd.Run(); err != nil {
		return errors.New("provideraccounts: update Claude Keychain credential")
	}
	return nil
}

func (securityClaudeKeychain) remove(configHome string, active bool) error {
	service, username, err := claudeKeychainIdentity(configHome, active)
	if err != nil {
		return err
	}
	cmd := exec.Command("security", "delete-generic-password", "-a", username, "-s", service)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
			return nil
		}
		return errors.New("provideraccounts: remove Claude Keychain credential")
	}
	return nil
}

// fileClaudeKeychain is the test/harness stand-in: each credential
// lives at <configHome>/.credentials.json — the exact layout the
// non-darwin file store uses — so darwin under `go test` behaves like
// every other platform. Subprocess provider mocks that write the
// credential file are visible to the app, and two Credentials over the
// same home share state the way production shares the one real
// Keychain, while t.TempDir()-rooted homes stay isolated from each
// other and from the developer's real login keychain. State lives on
// disk at caller-supplied paths, so there is no shared registry.
//
// Deliberate divergence from securityClaudeKeychain: reads are
// byte-exact (no TrimSpace) because the trim only exists to strip the
// newline security(1) appends to its own output.
type fileClaudeKeychain struct{}

func (fileClaudeKeychain) path(configHome string) string {
	return filepath.Join(configHome, claudeCredentialFileName)
}

func (f fileClaudeKeychain) read(configHome string, active bool) ([]byte, error) {
	snapshot, err := readCredentialSnapshot(f.path(configHome))
	if err != nil {
		// IsCredentialMissing accepts fs.ErrNotExist too, but callers
		// of the seam compare against ErrCredentialMissing like the
		// security backend returns.
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrCredentialMissing
		}
		return nil, err
	}
	return snapshot.Data, nil
}

func (f fileClaudeKeychain) present(configHome string, active bool) (bool, error) {
	info, err := os.Lstat(f.path(configHome))
	if err != nil {
		return false, nil
	}
	return info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && info.Size() > 0, nil
}

func (f fileClaudeKeychain) write(configHome string, active bool, data []byte) error {
	if err := validateClaudeKeychainWrite(data); err != nil {
		return err
	}
	path := f.path(configHome)
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("provideraccounts: credential path %s must not be a symlink", path)
	}
	return atomicfile.Write(path, data)
}

func (f fileClaudeKeychain) remove(configHome string, active bool) error {
	if err := os.Remove(f.path(configHome)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// claudeKeychainIdentity mirrors Claude Code's own Keychain naming: the
// canonical home (CLAUDE_CONFIG_DIR absent) uses the plain native
// service, every other config home hashes into a scoped service. The
// active flag — not path comparison — selects the plain name because
// Claude keys "is this the default home" off the variable being absent,
// not off its value (see AGENTS.md "Where a refresh is allowed to
// happen").
func claudeKeychainIdentity(configHome string, active bool) (string, string, error) {
	username := strings.TrimSpace(os.Getenv("USER"))
	if username == "" {
		current, err := user.Current()
		if err != nil {
			return "", "", fmt.Errorf("provideraccounts: resolve Keychain user: %w", err)
		}
		username = current.Username
	}
	service := "Claude Code-credentials"
	if !active {
		hash := sha256.Sum256([]byte(filepath.Clean(configHome)))
		service += "-" + hex.EncodeToString(hash[:4])
	}
	return service, username, nil
}
