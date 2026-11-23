package provideraccounts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/atomicfile"
)

const (
	agyCredentialFileName  = "antigravity-oauth-token"
	agyKeychainService     = "gemini"
	agyKeychainAccount     = "antigravity"
	goKeyringBase64Prefix  = "go-keyring-base64:"
	goKeyringEncodedPrefix = "go-keyring-encoded:"
)

// defaultAgyKeychain picks the Antigravity credential backend: security(1) on
// macOS desktop production runs, and the file-backed backend on Linux/WSL/Windows
// and inside `go test`.
func defaultAgyKeychain() providerKeychain {
	if testing.Testing() || runtime.GOOS != "darwin" {
		return fileAgyKeychain{}
	}
	return securityAgyKeychain{}
}

// securityAgyKeychain is the production macOS backend for Antigravity (`agy`).
//
// Unlike Claude Code (which scopes non-default config dirs into hashed Keychain
// services), agy uses a fixed zalando/go-keyring identity (service="gemini",
// account="antigravity") for its active login and writes a file when forced
// via AGY_ACP_FORCE_FILE_STORAGE=1 (used for ephemeral login/probe homes).
//
// Therefore:
//   - active == true reads/writes Keychain ("go-keyring-base64:" + base64(json))
//     with fallback to <configHome>/antigravity-oauth-token on exit 44 (absent),
//     mirroring agy's own fallback storage.
//   - active == false (saved account slots and ephemeral homes) always uses
//     <configHome>/antigravity-oauth-token directly.
type securityAgyKeychain struct{}

func (s securityAgyKeychain) read(configHome string, active bool) ([]byte, error) {
	if !active {
		return fileAgyKeychain{}.read(configHome, false)
	}
	source, err := s.loginSource(context.Background(), configHome, agyKeychainQueryTimeout)
	if err != nil {
		return nil, err
	}
	if source.kind != agyLoginInKeychain {
		// The file, read or failing to be read as it is: an unreadable one
		// answers with its own error.
		return fileAgyKeychain{}.read(configHome, true)
	}
	return s.readItem(configHome)
}

// readItem reads the Keychain item, and the fallback file when security(1)
// finds no item.
func (securityAgyKeychain) readItem(configHome string) ([]byte, error) {
	cmd, cmdErr := securityCommand("find-generic-password", "-a", agyKeychainAccount, "-w", "-s", agyKeychainService)
	if cmdErr != nil {
		return nil, cmdErr
	}
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
			return fileAgyKeychain{}.read(configHome, true)
		}
		return nil, fmt.Errorf("provideraccounts: read Antigravity Keychain credential: %v", err)
	}
	raw := bytes.TrimSpace(output)
	if len(raw) == 0 {
		return nil, ErrCredentialMissing
	}
	decoded, err := decodeGoKeyringPayload(raw)
	if err != nil {
		return nil, fmt.Errorf("provideraccounts: decode Antigravity Keychain credential: %w", err)
	}
	if len(decoded) == 0 {
		return nil, ErrCredentialMissing
	}
	if len(decoded) > maxCredentialBytes {
		return nil, errors.New("provideraccounts: Antigravity Keychain credential exceeds size limit")
	}
	return decoded, nil
}

// agyKeychainQueryTimeout bounds one attribute-only Keychain query (no -w,
// so it reads no secret and shows no access prompt) that decides where the
// canonical login is. It answers in well under a second; the bound keeps a
// stuck security(1) from holding a launch or a credential read, which then
// fails as a failed query.
const agyKeychainQueryTimeout = 10 * time.Second

type agyLoginSourceKind int

const (
	// agyLoginInKeychain is the Keychain item, or, with no fallback file,
	// agy's default store whether or not the item exists yet.
	agyLoginInKeychain agyLoginSourceKind = iota
	// agyLoginInFile is the fallback file, with no Keychain item.
	agyLoginInFile
	// agyLoginFileUnreadable is a fallback file that cannot be read as a
	// login, with no Keychain item.
	agyLoginFileUnreadable
)

// agyLoginSource is where the canonical login is read from.
type agyLoginSource struct {
	kind agyLoginSourceKind
	// unreadable is why the fallback file cannot be read, for
	// agyLoginFileUnreadable.
	unreadable error
}

// loginSource decides where the canonical login is read from, the one rule
// for read(configHome, true) and for the store an agy process for that login
// runs on: the Keychain item when there is one, else the fallback file. With
// no fallback file it is the Keychain and asks security(1) nothing. A
// Keychain query that fails, or gets no answer within timeout, is returned,
// never answered as either store.
func (s securityAgyKeychain) loginSource(ctx context.Context, configHome string, timeout time.Duration) (agyLoginSource, error) {
	path := fileAgyKeychain{}.path(configHome)
	info, statErr := os.Lstat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return agyLoginSource{kind: agyLoginInKeychain}, nil
	}
	itemPresent, err := s.itemPresent(ctx, timeout)
	if err != nil {
		return agyLoginSource{}, err
	}
	if itemPresent {
		return agyLoginSource{kind: agyLoginInKeychain}, nil
	}
	switch {
	case statErr != nil:
		return agyLoginSource{kind: agyLoginFileUnreadable, unreadable: statErr}, nil
	case info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular():
		return agyLoginSource{kind: agyLoginFileUnreadable, unreadable: fmt.Errorf("%s is not a regular file", path)}, nil
	case info.Size() == 0:
		return agyLoginSource{kind: agyLoginFileUnreadable, unreadable: fmt.Errorf("%s is empty", path)}, nil
	}
	return agyLoginSource{kind: agyLoginInFile}, nil
}

// canonicalFileStorage reports whether an agy process for the canonical
// login runs on the file store (true) or the Keychain (false): the store
// loginSource reads it from. A fallback file that cannot be read is an error,
// since read fails on it too.
func (s securityAgyKeychain) canonicalFileStorage(ctx context.Context, configHome string) (bool, error) {
	source, err := s.loginSource(ctx, configHome, agyKeychainQueryTimeout)
	if err != nil {
		return false, err
	}
	switch source.kind {
	case agyLoginInFile:
		return true, nil
	case agyLoginFileUnreadable:
		return false, fmt.Errorf("provideraccounts: the Keychain holds no Antigravity login and its fallback file cannot be read: %w", source.unreadable)
	default:
		return false, nil
	}
}

// itemPresent asks security(1) whether the Keychain item exists, without
// reading it, and gives up after timeout.
func (securityAgyKeychain) itemPresent(ctx context.Context, timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, errAgyKeychainQueryDeadline)
	defer cancel()
	cmd, cmdErr := securityCommandContext(ctx, "find-generic-password", "-a", agyKeychainAccount, "-s", agyKeychainService)
	if cmdErr != nil {
		return false, cmdErr
	}
	cmd.Stdout = nil
	cmd.Stderr = nil
	err := cmd.Run()
	if ctx.Err() != nil {
		if errors.Is(context.Cause(ctx), errAgyKeychainQueryDeadline) {
			return false, fmt.Errorf("provideraccounts: query Antigravity Keychain credential: security(1) gave no answer within %s", timeout)
		}
		return false, fmt.Errorf("provideraccounts: query Antigravity Keychain credential: %w", context.Cause(ctx))
	}
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
		return false, nil
	}
	return false, fmt.Errorf("provideraccounts: query Antigravity Keychain credential: %v", err)
}

var errAgyKeychainQueryDeadline = errors.New("the Antigravity Keychain query ran out of time")

func (s securityAgyKeychain) present(configHome string, active bool) (bool, error) {
	if !active {
		return fileAgyKeychain{}.present(configHome, false)
	}
	itemPresent, err := s.itemPresent(context.Background(), agyKeychainQueryTimeout)
	if err != nil || itemPresent {
		return itemPresent, err
	}
	return fileAgyKeychain{}.present(configHome, true)
}

func (s securityAgyKeychain) write(configHome string, active bool, data []byte) error {
	if len(data) == 0 || len(data) > maxCredentialBytes {
		return errors.New("provideraccounts: invalid Antigravity Keychain credential size")
	}
	if !active {
		return fileAgyKeychain{}.write(configHome, false, data)
	}
	encoded := encodeGoKeyringPayload(data)
	// encodeGoKeyringPayload emits pure printable ASCII ("go-keyring-base64:" +
	// std base64), so `-w` passes it directly through `security -i` stdin at
	// half the byte length of `-X` hex (keeping ~3 KiB raw OAuth credentials off
	// process argv).
	command := fmt.Sprintf(
		"add-generic-password -U -a %s -s %s -w %s\n",
		strconv.Quote(agyKeychainAccount),
		strconv.Quote(agyKeychainService),
		strconv.Quote(string(encoded)),
	)
	if len(command) > securityInteractiveLineLimit {
		// Never expose OAuth refresh tokens on process argv (KERN_PROCARGS2).
		return fmt.Errorf("provideraccounts: Antigravity Keychain credential (%d bytes encoded) exceeds security -i stdin limit", len(command))
	}
	cmd, cmdErr := securityCommand("-i")
	if cmdErr != nil {
		return cmdErr
	}
	cmd.Stdin = strings.NewReader(command)
	if err := cmd.Run(); err != nil {
		return errors.New("provideraccounts: update Antigravity Keychain credential")
	}
	// Remove any leftover file-backed active token so agy reads the newly
	// updated Keychain item cleanly without requiring AGY_ACP_FORCE_FILE_STORAGE.
	if err := (fileAgyKeychain{}).remove(configHome, true); err != nil {
		return fmt.Errorf("provideraccounts: remove stale Antigravity file token after Keychain write: %w", err)
	}
	return nil
}

func (securityAgyKeychain) remove(configHome string, active bool) error {
	if !active {
		return fileAgyKeychain{}.remove(configHome, false)
	}
	cmd, cmdErr := securityCommand("delete-generic-password", "-a", agyKeychainAccount, "-s", agyKeychainService)
	if cmdErr != nil {
		return cmdErr
	}
	var itemErr error
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 44 {
			itemErr = errors.New("provideraccounts: remove Antigravity Keychain credential")
		}
	}
	if fileErr := (fileAgyKeychain{}).remove(configHome, true); fileErr != nil && itemErr == nil {
		itemErr = fileErr
	}
	return itemErr
}

// encodeGoKeyringPayload encodes raw credential JSON in zalando/go-keyring's
// standard macOS format ("go-keyring-base64:<std-base64>").
func encodeGoKeyringPayload(raw []byte) []byte {
	dst := make([]byte, len(goKeyringBase64Prefix)+base64.StdEncoding.EncodedLen(len(raw)))
	copy(dst, goKeyringBase64Prefix)
	base64.StdEncoding.Encode(dst[len(goKeyringBase64Prefix):], raw)
	return dst
}

// decodeGoKeyringPayload decodes zalando/go-keyring's Keychain payload back
// into raw JSON bytes. Handles plain "go-keyring-base64:...", hex-wrapped
// security(1) output (returned when stored bytes contain newlines/non-ASCII),
// and raw UTF-8 JSON.
func decodeGoKeyringPayload(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	// security(1) -w prints raw hex when the item contains non-printable bytes.
	if len(trimmed)%2 == 0 && isHexBytes(trimmed) && trimmed[0] != '{' {
		if unhexed, err := hex.DecodeString(string(trimmed)); err == nil && len(unhexed) > 0 {
			trimmed = bytes.TrimSpace(unhexed)
		}
	}
	for _, prefix := range []string{goKeyringBase64Prefix, goKeyringEncodedPrefix} {
		if bytes.HasPrefix(trimmed, []byte(prefix)) {
			b64 := strings.TrimSpace(string(trimmed[len(prefix):]))
			decoded, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return nil, fmt.Errorf("decode %s: %w", prefix, err)
			}
			return decoded, nil
		}
	}
	return trimmed, nil
}

func isHexBytes(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// fileAgyKeychain is the test/harness stand-in storing credentials at
// <configHome>/antigravity-oauth-token.
type fileAgyKeychain struct{}

func (fileAgyKeychain) path(configHome string) string {
	return filepath.Join(configHome, agyCredentialFileName)
}

func (f fileAgyKeychain) read(configHome string, active bool) ([]byte, error) {
	snapshot, err := readCredentialSnapshot(f.path(configHome))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrCredentialMissing
		}
		return nil, err
	}
	return snapshot.Bytes(), nil
}

func (f fileAgyKeychain) present(configHome string, active bool) (bool, error) {
	info, err := os.Lstat(f.path(configHome))
	if err != nil {
		return false, nil
	}
	return info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && info.Size() > 0, nil
}

func (f fileAgyKeychain) write(configHome string, active bool, data []byte) error {
	if len(data) == 0 || len(data) > maxCredentialBytes {
		return errors.New("provideraccounts: invalid Antigravity Keychain credential size")
	}
	path := f.path(configHome)
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("provideraccounts: credential path %s must not be a symlink", path)
	}
	return atomicfile.Write(path, data)
}

func (f fileAgyKeychain) remove(configHome string, active bool) error {
	if err := os.Remove(f.path(configHome)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
