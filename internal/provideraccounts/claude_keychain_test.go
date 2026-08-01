package provideraccounts

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
