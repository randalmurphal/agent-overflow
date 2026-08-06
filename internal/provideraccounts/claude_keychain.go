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
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"

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
// construct securityClaudeKeychain in test code — the one sanctioned
// exception is claude_keychain_test.go's installFakeSecurity, which
// pins PATH to a directory holding only a stub `security` so the real
// binary is unreachable — and never add a code path that shells out to
// security(1) without going through this interface (pinned by
// TestNoSecurityCallsOutsideTheKeychainSeam).
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
//
// It composes fileClaudeKeychain as its FALLBACK, mirroring Claude
// Code's own fallbackStorage(keychain, plaintext) (verified against the
// 2.1.220 binary): the CLI reads the Keychain first and falls back to
// <configHome>/.credentials.json, and a non-transient Keychain-write
// failure (anything but a security(1) timeout) migrates the login to
// that file AND deletes the Keychain item. A darwin login can therefore
// be legitimately file-backed with no Keychain item at all — one locked
// keychain during an SSH-session token refresh is enough — so a
// Keychain-only read here would report a healthy login as missing.
type securityClaudeKeychain struct{}

// securityCommand builds every security(1) invocation this backend
// runs. Inside a test binary it refuses to resolve a system-installed
// security at all: the seam's testing.Testing() guard in
// defaultClaudeKeychain covers every fixture-built Credentials, but a
// test constructing securityClaudeKeychain directly (the
// installFakeSecurity pattern) is one forgotten PATH pin away from the
// developer's real login keychain (incident 2026-08-01). This makes
// that mistake fail loudly instead of executing.
func securityCommand(args ...string) (*exec.Cmd, error) {
	if testing.Testing() {
		resolved, err := exec.LookPath("security")
		if err != nil {
			return nil, fmt.Errorf("provideraccounts: resolve security(1) in test binary: %w", err)
		}
		for _, prefix := range []string{"/usr/", "/bin/", "/sbin/", "/System/", "/Library/", "/opt/"} {
			if strings.HasPrefix(resolved, prefix) {
				return nil, fmt.Errorf(
					"provideraccounts: test binary refused system security(1) at %s — stub it with installFakeSecurity first",
					resolved,
				)
			}
		}
	}
	return exec.Command("security", args...), nil
}

func (securityClaudeKeychain) read(configHome string, active bool) ([]byte, error) {
	service, username := claudeKeychainIdentity(configHome, active)
	cmd, cmdErr := securityCommand("find-generic-password", "-a", username, "-w", "-s", service)
	if cmdErr != nil {
		return nil, cmdErr
	}
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
			// Exit 44 ("no such item") is the one real answer of
			// "absent" — the credential may have migrated to the file.
			return fileClaudeKeychain{}.read(configHome, active)
		}
		// Any other failure — locked keychain, spawn failure — says
		// nothing about absence and must not collapse into it: a caller
		// like ActivateWithSnapshot treats "missing" as "nothing to
		// preserve", and a transient failure read as missing is how a
		// mid-switch rotation gets destroyed. (Claude Code's own read
		// additionally falls through to the file on exit 36 / locked;
		// we deliberately surface the error instead, because our
		// callers act on the answer rather than serving a session.)
		return nil, fmt.Errorf("provideraccounts: read Claude Keychain credential: %v", err)
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

// itemPresent answers whether the Keychain item itself exists, with no
// file fallback. Only exit 44 ("no such item") is a real answer of
// "absent". Every other failure — locked keychain, a spawn failure
// under load — is an error, NOT absence: account listings map a failed
// presence check to "still usable", and collapsing errors into false
// here painted saved accounts with a spurious "Sign in again" while the
// credential sat intact in the Keychain.
func (securityClaudeKeychain) itemPresent(service, username string) (bool, error) {
	// No -w: an attribute-only lookup never returns the secret, so the
	// probe costs a subprocess but not a credential read.
	cmd, cmdErr := securityCommand("find-generic-password", "-a", username, "-s", service)
	if cmdErr != nil {
		return false, cmdErr
	}
	cmd.Stdout = nil
	cmd.Stderr = nil
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
		return false, nil
	}
	return false, fmt.Errorf("provideraccounts: query Claude Keychain credential: %v", err)
}

func (s securityClaudeKeychain) present(configHome string, active bool) (bool, error) {
	service, username := claudeKeychainIdentity(configHome, active)
	itemPresent, err := s.itemPresent(service, username)
	if err != nil || itemPresent {
		return itemPresent, err
	}
	// Item definitively absent — the login may have migrated to the
	// credential file (see the type comment).
	return fileClaudeKeychain{}.present(configHome, active)
}

func (s securityClaudeKeychain) write(configHome string, active bool, data []byte) error {
	if err := validateClaudeKeychainWrite(data); err != nil {
		return err
	}
	service, username := claudeKeychainIdentity(configHome, active)
	// Claude Code deletes the credential file on the FIRST successful
	// Keychain write only (its issue #1414: when the Keychain already
	// held data, the file may be a copy deliberately shared with a
	// container and is left alone). Mirror that: record whether the
	// item existed before this write; an indeterminate answer skips the
	// cleanup rather than deleting a file whose role is unknown.
	itemExisted, presenceErr := s.itemPresent(service, username)
	hexValue := hex.EncodeToString(data)
	command := fmt.Sprintf(
		"add-generic-password -U -a %s -s %s -X %s\n",
		strconv.Quote(username),
		strconv.Quote(service),
		strconv.Quote(hexValue),
	)
	var cmd *exec.Cmd
	var cmdErr error
	if len(command) <= securityInteractiveLineLimit {
		cmd, cmdErr = securityCommand("-i")
		if cmdErr != nil {
			return cmdErr
		}
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
		cmd, cmdErr = securityCommand(
			"add-generic-password", "-U",
			"-a", username, "-s", service, "-X", hexValue,
		)
		if cmdErr != nil {
			return cmdErr
		}
	}
	if err := cmd.Run(); err != nil {
		return errors.New("provideraccounts: update Claude Keychain credential")
	}
	if presenceErr == nil && !itemExisted {
		// Best-effort, like Claude Code's own secondary.delete(): the
		// Keychain now holds the freshest bytes, so a leftover file is
		// a stale duplicate that would resurface as a dead login if the
		// item is ever deleted. The write itself already succeeded.
		_ = fileClaudeKeychain{}.remove(configHome, active)
	}
	return nil
}

func (securityClaudeKeychain) remove(configHome string, active bool) error {
	service, username := claudeKeychainIdentity(configHome, active)
	cmd, cmdErr := securityCommand("delete-generic-password", "-a", username, "-s", service)
	if cmdErr != nil {
		return cmdErr
	}
	var itemErr error
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 44 {
			itemErr = errors.New("provideraccounts: remove Claude Keychain credential")
		}
	}
	// A file-backed login (see the type comment) keeps its credential
	// in <configHome>/.credentials.json — removal must cover both
	// stores or a live token chain survives the account's deletion.
	if fileErr := (fileClaudeKeychain{}).remove(configHome, active); fileErr != nil && itemErr == nil {
		itemErr = fileErr
	}
	return itemErr
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

// claudeKeychainUsernamePattern is Claude Code's Keychain account-name
// validation (2.1.220): a derived username failing it is replaced with
// the literal "claude-code-user", never used raw. Mirroring the exact
// rule matters — a divergent account name means CC and AO each read and
// write their own item under the same service, silently forking the
// credential.
var claudeKeychainUsernamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// claudeKeychainIdentity mirrors Claude Code's own Keychain naming
// (verified against the 2.1.220 binary): the canonical home
// (CLAUDE_CONFIG_DIR absent) uses the plain native service, every other
// config home hashes into a scoped service. The active flag — not path
// comparison — selects the plain name because Claude keys "is this the
// default home" off the variable being absent, not off its value (see
// AGENTS.md "Where a refresh is allowed to happen"). The hash input is
// NFC-normalized exactly as CC normalizes it, and deliberately NOT
// path-cleaned — CC hashes the config-dir string as given, and the
// probe CLI must land its rotation in the same service AO reads back.
func claudeKeychainIdentity(configHome string, active bool) (string, string) {
	username := os.Getenv("USER")
	if username == "" {
		if current, err := user.Current(); err == nil {
			username = current.Username
		}
	}
	if !claudeKeychainUsernamePattern.MatchString(username) {
		username = "claude-code-user"
	}
	service := "Claude Code-credentials"
	if !active {
		hash := sha256.Sum256([]byte(norm.NFC.String(configHome)))
		service += "-" + hex.EncodeToString(hash[:4])
	}
	return service, username
}
