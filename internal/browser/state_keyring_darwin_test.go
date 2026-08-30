//go:build darwin

package browser

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	keyring "github.com/zalando/go-keyring"
)

// This opt-in test reaches the real login Keychain with a unique temporary
// item and deletes it. The ordinary deterministic suite uses injected keyring
// functions and never mutates a developer's Keychain.
func TestDarwinRealKeychainStateKey(t *testing.T) {
	if os.Getenv("AO_BROWSER_KEYCHAIN_INTEGRATION") != "1" {
		t.Skip("set AO_BROWSER_KEYCHAIN_INTEGRATION=1 to test the real macOS Keychain")
	}
	service := fmt.Sprintf("agent-overflow-browser-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	user := "site-data-test"
	_ = keyring.Delete(service, user)
	t.Cleanup(func() {
		if err := keyring.Delete(service, user); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			t.Errorf("delete temporary Keychain item: %v", err)
		}
	})

	first := newStateStore(t.TempDir())
	first.service, first.user = service, user
	key1, err := first.encryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.keyPath); !os.IsNotExist(err) {
		t.Fatalf("real Keychain path unexpectedly used fallback file: %v", err)
	}

	second := newStateStore(t.TempDir())
	second.service, second.user = service, user
	key2, err := second.encryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key1, key2) {
		t.Fatal("real Keychain did not return the stored browser key")
	}
}
