//go:build darwin

package darwinbundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundleIDUsesRootAndRunIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run")
	first := BundleID(root, "one")
	if !strings.HasPrefix(first, bundlePrefix) {
		t.Fatalf("id = %q, want harness prefix", first)
	}
	if first != BundleID(root, "one") {
		t.Fatal("same root and run identity produced different ids")
	}
	if first == BundleID(root, "two") {
		t.Fatal("different run identities shared a bundle id")
	}
	if first == BundleID(filepath.Join(t.TempDir(), "other"), "one") {
		t.Fatal("different roots shared a bundle id")
	}
}

func TestVerifyRejectsProductionBundle(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "x.app")
	executable := filepath.Join(app, "Contents", "MacOS", "agent-overflow")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0"?><plist><dict><key>CFBundleIdentifier</key><string>com.agentoverflow.app</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(executable, root, BundleID(root, "run"), "run"); err == nil {
		t.Fatal("Verify accepted a production bundle identifier")
	}
}

func TestVerifyAcceptsValidHarnessBundle(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "x.app")
	executable := filepath.Join(app, "Contents", "MacOS", "agent-overflow")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	id := BundleID(root, "run")
	plist := `<?xml version="1.0"?><plist><dict><key>CFBundleIdentifier</key><string>` + id + `</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(executable, root, id, "run"); err != nil {
		t.Fatalf("Verify rejected valid harness bundle: %v", err)
	}
}
