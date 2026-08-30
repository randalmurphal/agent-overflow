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

func TestCleanupRemovesOnlyValidatedHarnessBundleState(t *testing.T) {
	root := t.TempDir()
	library := t.TempDir()
	id := BundleID(root, "nonce")
	app := filepath.Join(root, ".ao-webview", id+".app")
	if err := os.MkdirAll(filepath.Join(app, "Contents"), 0o700); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0"?><plist><dict><key>CFBundleIdentifier</key><string>` + id + `</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(library, "WebKit", id),
		filepath.Join(library, "Caches", id),
		filepath.Join(library, "HTTPStorages", id),
		filepath.Join(library, "Saved Application State", id+".savedState"),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	preference := filepath.Join(library, "Preferences", id+".plist")
	if err := os.MkdirAll(filepath.Dir(preference), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preference, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(library, "WebKit", "com.agentoverflow.app")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := cleanupRoots(root, library, app); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{app, filepath.Join(library, "WebKit", id), filepath.Join(library, "Caches", id), preference} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("cleanup left %s: %v", path, err)
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("cleanup touched production state: %v", err)
	}
}

func TestCleanupRejectsMismatchedBundleID(t *testing.T) {
	root := t.TempDir()
	id := BundleID(root, "nonce")
	app := filepath.Join(root, ".ao-webview", id+".app")
	if err := os.MkdirAll(filepath.Join(app, "Contents"), 0o700); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0"?><plist><dict><key>CFBundleIdentifier</key><string>com.agentoverflow.app</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupRoots(root, t.TempDir(), app); err == nil {
		t.Fatal("cleanup accepted a production bundle id under a harness filename")
	}
	if _, err := os.Stat(app); err != nil {
		t.Fatalf("refused cleanup still removed app: %v", err)
	}
}
