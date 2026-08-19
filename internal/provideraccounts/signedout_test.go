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

// literalHuskDetector treats the literal bytes "husk" as Claude's sign-out
// marker, standing in for claude.CredentialsSignedOut without importing a
// provider package. Shared by every package test that exercises the refusal.
func literalHuskDetector(providerName string, data []byte) bool {
	return providerName == "claude" && string(data) == "husk"
}

// newHuskAwareCredentials builds a Credentials constructed with
// literalHuskDetector, the way production constructs with the real one.
func newHuskAwareCredentials(t *testing.T) *Credentials {
	t.Helper()
	credentials, err := NewCredentials(t.TempDir(), Policy{SignedOut: literalHuskDetector})
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

// A husk is a sign-out, never a credential. Every durable write refuses one,
// so no caller can persist the bytes that overwrite a saved login with
// nothing.
func TestCredentialWritesRefuseASignOutHusk(t *testing.T) {
	credentials := newHuskAwareCredentials(t)

	if err := credentials.WriteAccountCredential("claude", "slot", []byte("husk")); !errors.Is(
		err,
		ErrSignedOutCredential,
	) {
		t.Fatalf("WriteAccountCredential(husk) = %v, want ErrSignedOutCredential", err)
	}
	if err := credentials.CommitSelectedCredential("claude", "slot", []byte("husk")); !errors.Is(
		err,
		ErrSignedOutCredential,
	) {
		t.Fatalf("CommitSelectedCredential(husk) = %v, want ErrSignedOutCredential", err)
	}
	if err := credentials.WriteNativeCredentialForTest("claude", []byte("husk")); err != nil {
		// The provider-impersonating helper is the one path that may write a
		// husk: it stands in for the CLI blanking the canonical credential.
		t.Fatalf("WriteNativeCredentialForTest(husk) = %v, want the provider write allowed", err)
	}
	canonical, err := credentials.ReadCredential("claude", "", true)
	if err != nil || string(canonical) != "husk" {
		t.Fatalf("canonical credential = %s, %v, want the simulated provider husk", canonical, err)
	}

	// A provider that has no sign-out shape is unaffected.
	if err := credentials.WriteAccountCredential("codex", "slot", []byte("husk")); err != nil {
		t.Fatalf("WriteAccountCredential(codex) = %v, want the write allowed", err)
	}
}

// Refusing the husk is only half the guarantee: the bytes it would have
// replaced must still be there afterwards. A refusal that had already
// truncated the destination would be the same destroyed login with a nicer
// error message, and the slot's contents are the only copy of a single-use
// token chain.
func TestARefusedHuskLeavesTheExistingCredentialIntact(t *testing.T) {
	credentials := newHuskAwareCredentials(t)
	if err := credentials.WriteAccountCredential("claude", "slot", []byte("real-token")); err != nil {
		t.Fatal(err)
	}

	if err := credentials.WriteAccountCredential("claude", "slot", []byte("husk")); !errors.Is(
		err,
		ErrSignedOutCredential,
	) {
		t.Fatalf("WriteAccountCredential(husk) = %v, want ErrSignedOutCredential", err)
	}
	saved, err := credentials.ReadCredential("claude", "slot", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != "real-token" {
		t.Fatalf("slot = %s, want the refused write to have changed nothing", saved)
	}

	// The canonical store is the other destination, and the one whose loss
	// signs the user out of the running app.
	if err := credentials.CommitSelectedCredential("claude", "slot", []byte("real-token")); err != nil {
		t.Fatal(err)
	}
	if err := credentials.CommitSelectedCredential("claude", "slot", []byte("husk")); !errors.Is(
		err,
		ErrSignedOutCredential,
	) {
		t.Fatalf("CommitSelectedCredential(husk) = %v, want ErrSignedOutCredential", err)
	}
	canonical, err := credentials.ReadCredential("claude", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != "real-token" {
		t.Fatalf("canonical credential = %s, want the refused commit to have changed nothing", canonical)
	}
	saved, err = credentials.ReadCredential("claude", "slot", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != "real-token" {
		t.Fatalf("slot = %s, want the refused commit to have left it alone", saved)
	}
}

// The sign-out refusal is a chokepoint, and a chokepoint is only as good as
// the absence of ways around it. storeCredentialAt / storeActiveCredential are
// the raw writes the refusal wraps; a future in-package caller reaching for
// one — to "avoid the double check", or because a test helper made it look
// routine — would reopen exactly the hole that costs an account its login,
// with nothing failing to say so. This pins who may call them, in the style of
// TestNoSecurityCallsOutsideTheKeychainSeam.
func TestRawCredentialWritesStayBehindTheSignOutRefusal(t *testing.T) {
	allowedCallers := map[string]map[string]bool{
		"storeCredentialAt": {
			// The refusal itself, and the active-write wrapper below.
			"writeCredentialAt":     true,
			"storeActiveCredential": true,
		},
		"storeActiveCredential": {
			"writeActiveCredential": true,
			// The provider-impersonating helper: the CLI is the one actor
			// that legitimately writes a husk over the canonical credential.
			"WriteNativeCredentialForTest": true,
		},
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		scanned++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				allowed, watched := allowedCallers[selector.Sel.Name]
				if !watched || allowed[fn.Name.Name] {
					return true
				}
				t.Errorf(
					"%s: %s calls %s directly — route the write through the sign-out refusal instead",
					fset.Position(call.Pos()),
					fn.Name.Name,
					selector.Sel.Name,
				)
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatal("no package sources were scanned")
	}
}

// Installing a husk into the canonical store is the destructive half: it makes
// a dead login look active until the next request fails.
func TestActivateRefusesToInstallAHuskedSlot(t *testing.T) {
	credentials := newHuskAwareCredentials(t)
	if err := credentials.WriteNativeCredentialForTest("claude", []byte("husk")); err != nil {
		t.Fatal(err)
	}
	slot, err := credentials.AccountCredentialPath("claude", "dead")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(slot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slot, []byte("husk"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := credentials.Activate("claude", "", "dead"); !errors.Is(err, ErrSignedOutCredential) {
		t.Fatalf("Activate(husked slot) = %v, want ErrSignedOutCredential", err)
	}
}

// The preserve path claims never to store a husk into the outgoing slot. It
// must hold for a caller-supplied snapshot too — and the switch must still
// complete, because refusing it is what locked users out of every switch and
// delete in the 2026-08-03 incident.
func TestActivateWithSnapshotSkipsAHuskSnapshotAndStillSwitches(t *testing.T) {
	credentials := newHuskAwareCredentials(t)
	if err := credentials.WriteAccountCredential("claude", "outgoing", []byte("outgoing-saved")); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential("claude", "incoming", []byte("incoming-token")); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteNativeCredentialForTest("claude", []byte("husk")); err != nil {
		t.Fatal(err)
	}

	husk := CredentialSnapshot{Data: []byte("husk")}
	if err := credentials.ActivateWithSnapshot("claude", "outgoing", "incoming", &husk); err != nil {
		t.Fatalf("ActivateWithSnapshot() error = %v", err)
	}
	outgoing, err := credentials.ReadCredential("claude", "outgoing", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(outgoing) != "outgoing-saved" {
		t.Fatalf("outgoing slot = %s, want its last saved pair kept", outgoing)
	}
	canonical, err := credentials.ReadCredential("claude", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != "incoming-token" {
		t.Fatalf("canonical credential = %s, want the incoming account installed", canonical)
	}
}

// Seeding a temporary home with a husk buys a CLI run that can only fail, and
// leaves a registry entry pointing at an orphan that was never a login.
func TestNewEphemeralHomeWithCredentialRefusesAHusk(t *testing.T) {
	credentials := newHuskAwareCredentials(t)
	home, err := credentials.NewEphemeralHomeWithCredential("claude", []byte("husk"), "dead")
	if !errors.Is(err, ErrSignedOutCredential) {
		if home != nil {
			_ = home.Cleanup()
		}
		t.Fatalf("NewEphemeralHomeWithCredential(husk) = %v, want ErrSignedOutCredential", err)
	}
	if home != nil {
		t.Fatal("a temporary home was created for a sign-out seed")
	}
}

// A husked slot captures as "no credential", so the restore removes the husk
// instead of rewriting it: a slot without a credential and a husked slot are
// the same "needs login" state, and only one of them lies about it.
func TestCaptureAccountCredentialTreatsAHuskAsNoCredential(t *testing.T) {
	credentials := newHuskAwareCredentials(t)
	if err := credentials.WriteAccountCredential("claude", "dead", []byte("real")); err != nil {
		t.Fatal(err)
	}
	slot, err := credentials.AccountCredentialPath("claude", "dead")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slot, []byte("husk"), 0o600); err != nil {
		t.Fatal(err)
	}

	saved, err := credentials.CaptureAccountCredential("claude", "dead")
	if err != nil {
		t.Fatal(err)
	}
	if saved.HadCredential() {
		t.Fatal("a husked slot captured as holding a credential")
	}
	if err := credentials.RestoreAccountCredential(saved); err != nil {
		t.Fatalf("RestoreAccountCredential() error = %v", err)
	}
	if _, err := os.Lstat(slot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("husk credential still present after restore: %v", err)
	}
	// The slot directory predates the capture, so it survives.
	if _, err := os.Lstat(filepath.Dir(slot)); err != nil {
		t.Fatalf("restore removed a slot directory it did not create: %v", err)
	}
}

// CredentialUsable is the question the account list asks: present AND alive.
func TestCredentialUsableSeparatesAliveFromHuskedAndMissing(t *testing.T) {
	credentials := newHuskAwareCredentials(t)
	if err := credentials.WriteAccountCredential("claude", "alive", []byte("token")); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential("claude", "dead", []byte("token")); err != nil {
		t.Fatal(err)
	}
	dead, err := credentials.AccountCredentialPath("claude", "dead")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dead, []byte("husk"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		accountID string
		want      bool
	}{
		{"alive", true},
		{"dead", false},
		{"never-saved", false},
	} {
		got, err := credentials.CredentialUsable("claude", tc.accountID, false)
		if err != nil {
			t.Fatalf("CredentialUsable(%q): %v", tc.accountID, err)
		}
		if got != tc.want {
			t.Fatalf("CredentialUsable(%q) = %v, want %v", tc.accountID, got, tc.want)
		}
	}

	// An unreadable slot is a failure to find out, not a verdict.
	if _, err := credentials.CredentialUsable("claude", "../escape", false); err == nil {
		t.Fatal("CredentialUsable() accepted an account id that is not a safe path component")
	}
}

// Without a detector installed no provider claims a sign-out shape, so the
// wrapper must answer false rather than panic on the nil hook.
func TestCredentialSignedOutWithoutADetector(t *testing.T) {
	credentials, err := NewCredentials(t.TempDir(), Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if credentials.CredentialSignedOut("claude", []byte("husk")) {
		t.Fatal("CredentialSignedOut() reported a sign-out with no detector installed")
	}
}
