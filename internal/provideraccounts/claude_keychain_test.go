package provideraccounts

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The active slot's Keychain service name is fixed regardless of which
// userHome a Credentials was built with, so a real security(1) backend
// inside a test binary targets the developer's actual Claude Code login
// — no temporary directory isolates it. Incident 2026-08-01: `make
// go-test` on a dev Mac overwrote the developer's real login with
// fixture bytes. This pins the guard: under `go test`, NewCredentials
// must install the file-backed stand-in.
func TestNewCredentialsInstallsFileKeychainUnderGoTest(t *testing.T) {
	credentials, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := credentials.keychain.(fileClaudeKeychain); !ok {
		t.Fatalf(
			"test-binary Credentials got keychain %T, want fileClaudeKeychain — tests must never reach security(1)",
			credentials.keychain,
		)
	}
}

// NewCredentialsWithFileKeychain is the harness's explicit opt-in to
// the same stand-in for processes that are not test binaries.
func TestNewCredentialsWithFileKeychainInstallsTheStandIn(t *testing.T) {
	credentials, err := NewCredentialsWithFileKeychain(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := credentials.keychain.(fileClaudeKeychain); !ok {
		t.Fatalf("harness Credentials got keychain %T, want fileClaudeKeychain", credentials.keychain)
	}
}

// The stand-in must mirror the parts of the security(1) contract that
// callers rely on: missing reads return ErrCredentialMissing, distinct
// config homes are distinct credentials, removal is idempotent (exit-44
// tolerance), size validation matches, and present tracks readability
// without returning bytes.
func TestFileClaudeKeychainMirrorsSecurityBackendContract(t *testing.T) {
	kc := fileClaudeKeychain{}
	homeA := t.TempDir()
	homeB := t.TempDir()

	if _, err := kc.read(homeA, false); !errors.Is(err, ErrCredentialMissing) || !IsCredentialMissing(err) {
		t.Fatalf("empty read error = %v, want ErrCredentialMissing", err)
	}
	if present, err := kc.present(homeA, false); err != nil || present {
		t.Fatalf("present on empty home = %v, %v; want false, nil", present, err)
	}

	if err := kc.write(homeA, false, []byte("slot-a")); err != nil {
		t.Fatalf("write slot A: %v", err)
	}
	slot, err := kc.read(homeA, false)
	if err != nil || string(slot) != "slot-a" {
		t.Fatalf("slot A read = %q, %v", slot, err)
	}
	if present, err := kc.present(homeA, false); err != nil || !present {
		t.Fatalf("present after write = %v, %v; want true, nil", present, err)
	}

	// Distinct config homes are distinct credentials (the security
	// backend scopes by hashed service; this backend scopes by path).
	if _, err := kc.read(homeB, false); !errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("home B read leaked another home's credential: %v", err)
	}

	if err := kc.write(homeA, false, nil); err == nil {
		t.Fatal("empty write must be rejected")
	}
	if err := kc.write(homeA, false, []byte(strings.Repeat("x", maxCredentialBytes+1))); err == nil {
		t.Fatal("oversized write must be rejected")
	}

	if err := kc.remove(homeA, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := kc.remove(homeA, false); err != nil {
		t.Fatalf("second remove must be idempotent like exit-44: %v", err)
	}
	if _, err := kc.read(homeA, false); !errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("removed credential still readable: %v", err)
	}
}

// The stand-in lays credentials out exactly like the non-darwin file
// store, so a subprocess (a mock provider) writing the credential file
// is visible through the seam — the property the darwin test suite
// depends on to exercise external-login scenarios without skips.
func TestFileClaudeKeychainSharesTheNonDarwinLayout(t *testing.T) {
	kc := fileClaudeKeychain{}
	home := t.TempDir()

	if err := os.WriteFile(filepath.Join(home, claudeCredentialFileName), []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := kc.read(home, true)
	if err != nil || string(data) != "external" {
		t.Fatalf("read of externally written file = %q, %v", data, err)
	}

	if err := kc.write(home, true, []byte("internal")); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(home, claudeCredentialFileName))
	if err != nil || string(onDisk) != "internal" {
		t.Fatalf("on-disk bytes after seam write = %q, %v", onDisk, err)
	}
}

// Symlinked credential paths are rejected on both read and write, like
// the non-darwin store.
func TestFileClaudeKeychainRejectsSymlinkedCredentialPaths(t *testing.T) {
	kc := fileClaudeKeychain{}
	home := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, claudeCredentialFileName)); err != nil {
		t.Fatal(err)
	}

	if err := kc.write(home, true, []byte("payload")); err == nil {
		t.Fatal("write through a symlink must be rejected")
	}
	if _, err := kc.read(home, true); err == nil || errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("symlink read error = %v, want a non-missing failure", err)
	}
	if present, err := kc.present(home, true); err != nil || present {
		t.Fatalf("present on symlink = %v, %v; want false, nil", present, err)
	}
}

// State lives on disk, so two Credentials over the SAME home share it —
// the way production instances share the one real Keychain (pinned
// because a per-instance fake once silently vacuated initStores
// coverage) — while separate t.TempDir() homes stay isolated.
func TestFileKeychainStateFollowsTheHomeNotTheInstance(t *testing.T) {
	sharedHome := t.TempDir()
	first, err := NewCredentials(sharedHome)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCredentials(sharedHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.WriteNativeCredentialForTest("claude", []byte("shared-active")); err != nil {
		t.Fatal(err)
	}
	data, err := second.ReadCredential("claude", "", true)
	if err != nil || string(data) != "shared-active" {
		t.Fatalf("second instance over the same home read = %q, %v; want shared-active", data, err)
	}

	other, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ReadCredential("claude", "", true); !IsCredentialMissing(err) {
		t.Fatalf("instance over a different home sees foreign credential: %v", err)
	}
}

// installFakeSecurity makes the security(1) backend testable without a
// real keychain: PATH is pinned to a directory holding ONLY a stub
// `security`, so the real binary is unreachable no matter what the seam
// resolves (the sanctioned exception to "never construct
// securityClaudeKeychain in test code" — see the seam comment). The
// stub's behavior is driven by FAKE_SECURITY_* environment variables
// set per scenario via t.Setenv.
func installFakeSecurity(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake security(1) is an sh script")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
find-generic-password)
  wantw=0
  for a in "$@"; do [ "$a" = "-w" ] && wantw=1; done
  code="${FAKE_SECURITY_FIND_EXIT:-0}"
  if [ "$code" -eq 0 ] && [ "$wantw" -eq 1 ]; then printf '%s\n' "$FAKE_SECURITY_READ_PAYLOAD"; fi
  exit "$code" ;;
add-generic-password) exit "${FAKE_SECURITY_WRITE_EXIT:-0}" ;;
delete-generic-password) exit "${FAKE_SECURITY_DELETE_EXIT:-0}" ;;
-i) read -r _line; exit "${FAKE_SECURITY_WRITE_EXIT:-0}" ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "security"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// Even a test that constructs securityClaudeKeychain directly — the
// one mistake the defaultClaudeKeychain guard cannot catch — must not
// be able to reach a system-installed security(1): every method routes
// through securityCommand, which refuses system paths inside test
// binaries. present() is the probe here because it is attribute-only,
// so if the guard ever regressed the worst case is a harmless lookup,
// never a mutation or a secret read.
func TestSecurityBackendRefusesSystemSecurityInTestBinaries(t *testing.T) {
	kc := securityClaudeKeychain{}
	if _, err := kc.present(t.TempDir(), false); err == nil {
		t.Fatal("securityClaudeKeychain without a stubbed PATH must refuse to run inside a test binary")
	}
}

// Only exit 44 ("no such item") may read as absence. Every other
// security(1) failure — locked keychain, spawn trouble — must surface
// as an error: ActivateWithSnapshot treats "missing" as "nothing to
// preserve", so a transient failure collapsed into ErrCredentialMissing
// is how a mid-switch rotation of a single-use chain gets destroyed.
func TestSecurityKeychainReadDistinguishesAbsenceFromFailure(t *testing.T) {
	installFakeSecurity(t)
	kc := securityClaudeKeychain{}
	home := t.TempDir()

	t.Setenv("FAKE_SECURITY_FIND_EXIT", "44")
	if _, err := kc.read(home, false); !errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("exit-44 read error = %v, want ErrCredentialMissing", err)
	}

	t.Setenv("FAKE_SECURITY_FIND_EXIT", "1")
	if _, err := kc.read(home, false); err == nil || errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("non-44 read error = %v, want a non-missing failure", err)
	}
	if present, err := kc.present(home, false); err == nil || present {
		t.Fatalf("non-44 present = %v, %v; want an error, not absence", present, err)
	}

	t.Setenv("FAKE_SECURITY_FIND_EXIT", "0")
	t.Setenv("FAKE_SECURITY_READ_PAYLOAD", "item-bytes")
	data, err := kc.read(home, false)
	if err != nil || string(data) != "item-bytes" {
		t.Fatalf("successful read = %q, %v", data, err)
	}
}

// Claude Code's fallbackStorage (verified on the 2.1.220 binary) can
// legitimately migrate a login to <configHome>/.credentials.json and
// DELETE the keychain item — any non-timeout keychain-write failure
// does it, one locked keychain during an SSH refresh is enough. A
// definitively absent item must therefore fall back to the file before
// answering "missing", or a healthy file-backed login reads as signed
// out.
func TestSecurityKeychainFallsBackToTheCredentialFile(t *testing.T) {
	installFakeSecurity(t)
	kc := securityClaudeKeychain{}
	home := t.TempDir()
	t.Setenv("FAKE_SECURITY_FIND_EXIT", "44")

	if present, err := kc.present(home, false); err != nil || present {
		t.Fatalf("present with neither store = %v, %v; want false, nil", present, err)
	}

	filePath := filepath.Join(home, claudeCredentialFileName)
	if err := os.WriteFile(filePath, []byte("file-backed-login"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := kc.read(home, false)
	if err != nil || string(data) != "file-backed-login" {
		t.Fatalf("file-fallback read = %q, %v", data, err)
	}
	if present, err := kc.present(home, false); err != nil || !present {
		t.Fatalf("file-fallback present = %v, %v; want true, nil", present, err)
	}
}

// The write mirrors Claude Code's migration cleanup exactly: the
// credential file is deleted only when the write CREATED the item
// (migrating the login back into the keychain). When the item already
// existed the file is left alone — Claude Code keeps it deliberately
// (its issue #1414: the file may be shared with a container) — and a
// failed write touches nothing.
func TestSecurityKeychainWriteMirrorsClaudeCodeMigrationCleanup(t *testing.T) {
	installFakeSecurity(t)
	kc := securityClaudeKeychain{}

	writeCases := []struct {
		name         string
		findExit     string
		writeExit    string
		wantErr      bool
		wantFileKept bool
	}{
		{"migration back into the keychain deletes the file", "44", "0", false, false},
		{"existing item leaves the shared file alone", "0", "0", false, true},
		{"failed write touches nothing", "44", "1", true, true},
	}
	for _, tc := range writeCases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			filePath := filepath.Join(home, claudeCredentialFileName)
			if err := os.WriteFile(filePath, []byte("stale-file-copy"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_SECURITY_FIND_EXIT", tc.findExit)
			t.Setenv("FAKE_SECURITY_WRITE_EXIT", tc.writeExit)

			err := kc.write(home, false, []byte("fresh-bytes"))
			if (err != nil) != tc.wantErr {
				t.Fatalf("write error = %v, wantErr %v", err, tc.wantErr)
			}
			_, statErr := os.Lstat(filePath)
			if kept := statErr == nil; kept != tc.wantFileKept {
				t.Fatalf("file kept = %v, want %v", kept, tc.wantFileKept)
			}
		})
	}
}

// Removal must cover both stores: a file-backed login whose removal
// only deleted the (absent) keychain item would leave a live token
// chain on disk after the account was deleted.
func TestSecurityKeychainRemoveCoversBothStores(t *testing.T) {
	installFakeSecurity(t)
	kc := securityClaudeKeychain{}
	home := t.TempDir()
	filePath := filepath.Join(home, claudeCredentialFileName)
	if err := os.WriteFile(filePath, []byte("file-backed-login"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FAKE_SECURITY_DELETE_EXIT", "44")
	if err := kc.remove(home, false); err != nil {
		t.Fatalf("remove with absent item = %v, want nil (exit-44 tolerance)", err)
	}
	if _, err := os.Lstat(filePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential file survived removal: %v", err)
	}

	t.Setenv("FAKE_SECURITY_DELETE_EXIT", "1")
	if err := kc.remove(home, false); err == nil {
		t.Fatal("remove with a real security failure must error")
	}
}

// The naming mirror must track Claude Code exactly (2.1.220): a
// username failing CC's validation becomes the literal
// "claude-code-user" (a divergent account name silently forks the
// credential into two items), and the service hash input is
// NFC-normalized so a decomposed path names the same service CC uses.
func TestClaudeKeychainIdentityMirrorsClaudeCodeSanitization(t *testing.T) {
	t.Setenv("USER", "has space!")
	if _, username := claudeKeychainIdentity("/tmp/x", true); username != "claude-code-user" {
		t.Fatalf("invalid USER sanitized to %q, want claude-code-user", username)
	}

	t.Setenv("USER", "ok_user-1.dev")
	if _, username := claudeKeychainIdentity("/tmp/x", true); username != "ok_user-1.dev" {
		t.Fatalf("valid USER = %q, want passthrough", username)
	}

	composed, _ := claudeKeychainIdentity("/tmp/caf\u00e9", false)
	decomposed, _ := claudeKeychainIdentity("/tmp/cafe\u0301", false)
	if composed != decomposed {
		t.Fatalf("NFC (%q) and NFD (%q) spellings must hash to one service", composed, decomposed)
	}
	other, _ := claudeKeychainIdentity("/tmp/other", false)
	if other == composed {
		t.Fatal("distinct homes must hash to distinct services")
	}
}

// Every security(1) invocation must live inside claude_keychain.go —
// the seam that keeps test binaries and harness runs away from the
// developer's real login keychain (incident 2026-08-01). A call added
// anywhere else would bypass the testing.Testing() guard silently.
func TestNoSecurityCallsOutsideTheKeychainSeam(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	allowedFile := filepath.Join(root, "internal", "provideraccounts", "claude_keychain.go")

	skipDirs := map[string]bool{
		".git": true, ".claude": true, "node_modules": true,
		"frontend": true, "docs": true, "dist": true, "build": true,
	}
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || path == allowedFile {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING || literal.Value != `"security"` {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok && strings.HasPrefix(selector.Sel.Name, "Command") {
				t.Errorf(
					"%s: exec of security(1) outside the claudeKeychain seam — route it through claude_keychain.go",
					fset.Position(call.Pos()),
				)
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}
