package provideraccounts

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGoKeyringPayloadRoundTrip(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"auth_method":"oauth","id_token_email":"dev@example.com","token":{"access_token":"tok"}}`)
	encoded := encodeGoKeyringPayload(raw)
	if want := goKeyringBase64Prefix + base64.StdEncoding.EncodeToString(raw); string(encoded) != want {
		t.Fatalf("encoded = %q, want %q", encoded, want)
	}

	got, err := decodeGoKeyringPayload(encoded)
	if err != nil {
		t.Fatalf("decode plain go-keyring: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("decoded = %q, want %q", got, raw)
	}

	// Hex-wrapped security(1) output for payloads containing non-ASCII or newlines.
	hexWrapped := []byte(hex.EncodeToString(encoded))
	gotHex, err := decodeGoKeyringPayload(hexWrapped)
	if err != nil {
		t.Fatalf("decode hex-wrapped go-keyring: %v", err)
	}
	if string(gotHex) != string(raw) {
		t.Fatalf("hex decoded = %q, want %q", gotHex, raw)
	}

	// Raw JSON passes through unchanged.
	gotRaw, err := decodeGoKeyringPayload(raw)
	if err != nil {
		t.Fatalf("decode raw JSON: %v", err)
	}
	if string(gotRaw) != string(raw) {
		t.Fatalf("raw decoded = %q, want %q", gotRaw, raw)
	}
}

func TestSecurityAgyKeychainRefusesSystemSecurityInTestBinaries(t *testing.T) {
	kc := securityAgyKeychain{}
	if _, err := kc.present(t.TempDir(), true); err == nil {
		t.Fatal("securityAgyKeychain without stubbed PATH must refuse to run inside a test binary")
	}
}

func TestSecurityAgyKeychainReadFallbackWriteRemove(t *testing.T) {
	installFakeSecurity(t)
	home := t.TempDir()
	raw := []byte(`{"id_token_email":"user@example.com","token":{"access_token":"a1","refresh_token":"r1"}}`)
	encoded := string(encodeGoKeyringPayload(raw))

	// Active read decodes go-keyring-base64 payload from Keychain.
	t.Setenv("FAKE_SECURITY_FIND_EXIT", "0")
	t.Setenv("FAKE_SECURITY_READ_PAYLOAD", encoded)
	kc := securityAgyKeychain{}
	got, err := kc.read(home, true)
	if err != nil {
		t.Fatalf("active read: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("active read = %q, want %q", got, raw)
	}

	// Non-44 error surfaces as an error rather than missing.
	t.Setenv("FAKE_SECURITY_FIND_EXIT", "36")
	if _, err := kc.read(home, true); err == nil || IsCredentialMissing(err) {
		t.Fatalf("exit 36 read err = %v, want non-missing error", err)
	}
	if _, err := kc.present(home, true); err == nil {
		t.Fatal("exit 36 present must return error")
	}

	// Exit 44 falls back to file-backed active token.
	t.Setenv("FAKE_SECURITY_FIND_EXIT", "44")
	if _, err := kc.read(home, true); !IsCredentialMissing(err) {
		t.Fatalf("exit 44 without file = %v, want ErrCredentialMissing", err)
	}
	if err := os.WriteFile(filepath.Join(home, agyCredentialFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	gotFallback, err := kc.read(home, true)
	if err != nil || string(gotFallback) != string(raw) {
		t.Fatalf("exit 44 fallback = %q, %v; want %q", gotFallback, err, raw)
	}
	presentFallback, err := kc.present(home, true)
	if err != nil || !presentFallback {
		t.Fatalf("exit 44 present fallback = %v, %v; want true, nil", presentFallback, err)
	}

	// Active write updates Keychain and cleans up any stale active file.
	t.Setenv("FAKE_SECURITY_WRITE_EXIT", "0")
	if err := kc.write(home, true, raw); err != nil {
		t.Fatalf("active write: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, agyCredentialFileName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected stale active token file removed after Keychain write, stat err=%v", statErr)
	}

	// Non-active slots always use file storage, never Keychain.
	slotDir := t.TempDir()
	if err := kc.write(slotDir, false, raw); err != nil {
		t.Fatalf("slot write: %v", err)
	}
	gotSlot, err := kc.read(slotDir, false)
	if err != nil || string(gotSlot) != string(raw) {
		t.Fatalf("slot read = %q, %v; want %q", gotSlot, err, raw)
	}

	// Active remove deletes both Keychain item and fallback file.
	if err := os.WriteFile(filepath.Join(home, agyCredentialFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_SECURITY_DELETE_EXIT", "0")
	if err := kc.remove(home, true); err != nil {
		t.Fatalf("active remove: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, agyCredentialFileName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("fallback file survived active remove: %v", statErr)
	}
}

func TestFileAgyKeychainIsolationAndSymlinkRejection(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	kc := fileAgyKeychain{}
	payload := []byte(`{"token":{"access_token":"ok"}}`)

	if err := kc.write(home, true, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := kc.read(home, true)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("read = %q, %v", got, err)
	}
	if err := kc.remove(home, true); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := kc.read(home, true); !IsCredentialMissing(err) {
		t.Fatalf("read after remove = %v, want missing", err)
	}

	if runtime.GOOS == "windows" {
		return
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, agyCredentialFileName)); err != nil {
		t.Fatal(err)
	}
	if err := kc.write(home, true, payload); err == nil {
		t.Fatal("write through symlink must be rejected")
	}
	if present, err := kc.present(home, true); err != nil || present {
		t.Fatalf("present on symlink = %v, %v; want false, nil", present, err)
	}
}
