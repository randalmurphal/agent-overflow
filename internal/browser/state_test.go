package browser

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/chromedp/cdproto/network"
)

func TestStateStoreEncryptedRoundTrip(t *testing.T) {
	root := t.TempDir()
	store := newTestStateStore(root, bytes.Repeat([]byte{7}, 32))
	want := storageState{
		Workspace:    "/repo",
		Cookies:      []*network.CookieParam{{Name: "session", Value: "secret", Domain: "example.com", Path: "/"}},
		LocalStorage: map[string]map[string]string{"https://example.com": {"token": "private"}},
	}
	if err := store.save(want); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.path("/repo"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("secret")) || bytes.Contains(raw, []byte("private")) {
		t.Fatalf("persisted browser state is plaintext: %s", raw)
	}
	got, err := store.load("/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cookies) != 1 || got.Cookies[0].Value != "secret" || got.LocalStorage["https://example.com"]["token"] != "private" {
		t.Fatalf("round trip = %#v", got)
	}
}

func TestStateStoreClearPreservesFallbackKey(t *testing.T) {
	root := t.TempDir()
	store := newStateStore(root)
	store.keyFn = store.loadOrCreateFallbackKey
	if err := store.save(storageState{Workspace: "/repo"}); err != nil {
		t.Fatal(err)
	}
	if err := store.clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "browser-state.key")); err != nil {
		t.Fatalf("fallback key removed by site-data clear: %v", err)
	}
	if _, err := os.Stat(store.root); !os.IsNotExist(err) {
		t.Fatalf("state directory still exists: %v", err)
	}
}

func TestAuthorizeFileStaysWithinGrantedRoots(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "index.html")
	if err := os.WriteFile(inside, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: Config{}, scopes: make(map[string]*workspaceScope)}
	access := Access{ThreadID: "t", Workspace: root}
	wantInside, _ := filepath.EvalSymlinks(inside)
	if got, err := manager.authorizeFile(access, inside); err != nil || got != wantInside {
		t.Fatalf("inside = %q, %v", got, err)
	}
	if _, err := manager.authorizeFile(access, outside); err == nil {
		t.Fatal("outside file unexpectedly allowed")
	}
	manager.config.AllowOutsideWorkspace = true
	wantOutside, _ := filepath.EvalSymlinks(outside)
	if got, err := manager.authorizeFile(access, outside); err != nil || got != wantOutside {
		t.Fatalf("outside with grant = %q, %v", got, err)
	}
}
