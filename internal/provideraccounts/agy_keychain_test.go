package provideraccounts

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

func TestAgyFileKeychainForceStorageAndEncodedPrefix(t *testing.T) {
	t.Parallel()
	// 1. decodeGoKeyringPayload supports both go-keyring-base64: and go-keyring-encoded:.
	raw := `{"token":{"access_token":"abc"}}`
	encoded := []byte(goKeyringEncodedPrefix + base64.StdEncoding.EncodeToString([]byte(raw)))
	got, err := decodeGoKeyringPayload(encoded)
	if err != nil || string(got) != raw {
		t.Fatalf("decodeGoKeyringPayload(encoded) = %q, %v; want %q", got, err, raw)
	}

	// 2. A file-backed store reads the canonical login from the file, so agy
	// is launched on file storage, before and after the file exists, without
	// ever invoking security(1).
	home := t.TempDir()
	creds, err := NewCredentialsWithFileKeychain(home, Policy{})
	if err != nil {
		t.Fatalf("NewCredentialsWithFileKeychain: %v", err)
	}
	if fileStorage, err := creds.AgyCanonicalFileStorage(context.Background()); err != nil || !fileStorage {
		t.Fatalf("file-backed store before the file = %v, %v; want file storage", fileStorage, err)
	}
	appData := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(appData, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appData, agyCredentialFileName), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if fileStorage, err := creds.AgyCanonicalFileStorage(context.Background()); err != nil || !fileStorage {
		t.Fatalf("file-backed store with the file = %v, %v; want file storage", fileStorage, err)
	}
}

// The macOS backend decides once where the canonical login is, and the
// reader and the store an agy process runs on both follow it. For each
// result: what the reader returns (as on main), and the store a launch gets.
func TestSecurityAgyKeychainReaderAndLaunchFollowOneSource(t *testing.T) {
	installFakeSecurity(t)
	kc := securityAgyKeychain{}
	raw := []byte(`{"id_token_email":"file@example.com","token":{"access_token":"f1","refresh_token":"rf"}}`)
	item := []byte(`{"id_token_email":"keychain@example.com","token":{"access_token":"k1","refresh_token":"rk"}}`)
	t.Setenv("FAKE_SECURITY_READ_PAYLOAD", string(encodeGoKeyringPayload(item)))
	ctx := context.Background()

	cases := []struct {
		name string
		// file writes the fallback file into home; nil writes none.
		file func(t *testing.T, home string)
		exit string
		// read is the login the reader returns, "" for an error, and
		// "missing" for ErrCredentialMissing.
		read string
		// store is "keychain", "file" or "" for a launch that fails.
		store string
	}{
		{name: "item, no file", exit: "0", read: string(item), store: "keychain"},
		{name: "no item, no file", exit: "44", read: "missing", store: "keychain"},
		{name: "item and file", file: writeFile(raw), exit: "0", read: string(item), store: "keychain"},
		{name: "file, no item", file: writeFile(raw), exit: "44", read: string(raw), store: "file"},
		{name: "empty file, no item", file: writeFile(nil), exit: "44", read: "", store: ""},
		{name: "symlinked file, no item", file: symlinkFile(raw), exit: "44", read: "", store: ""},
		{name: "symlinked file, item", file: symlinkFile(raw), exit: "0", read: string(item), store: "keychain"},
		{name: "file, query fails", file: writeFile(raw), exit: "36", read: "", store: ""},
		{name: "oversized file, no item", file: writeFile(make([]byte, maxCredentialBytes+1)), exit: "44", read: "", store: ""},
		{name: "oversized file, item", file: writeFile(make([]byte, maxCredentialBytes+1)), exit: "0", read: string(item), store: "keychain"},
		{name: "unopenable file, no item", file: unopenableFile(raw), exit: "44", read: "", store: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			if c.file != nil {
				c.file(t, home)
			}
			t.Setenv("FAKE_SECURITY_FIND_EXIT", c.exit)
			got, err := kc.read(home, true)
			switch c.read {
			case "":
				if err == nil {
					t.Errorf("read = %s, want an error", got)
				}
			case "missing":
				if !errors.Is(err, ErrCredentialMissing) {
					t.Errorf("read = %s, %v; want the login missing", got, err)
				}
			default:
				if err != nil || string(got) != c.read {
					t.Errorf("read = %s, %v; want %s", got, err, c.read)
				}
			}
			fileStorage, err := kc.canonicalFileStorage(ctx, home, agyKeychainQueryTimeout)
			store := "keychain"
			if fileStorage {
				store = "file"
			}
			if err != nil {
				store = ""
			}
			if store != c.store {
				t.Errorf("launch store = %q (%v), want %q", store, err, c.store)
			}
		})
	}
}

func writeFile(data []byte) func(t *testing.T, home string) {
	return func(t *testing.T, home string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, agyCredentialFileName), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// unopenableFile is a fallback file nobody may open.
func unopenableFile(data []byte) func(t *testing.T, home string) {
	return func(t *testing.T, home string) {
		t.Helper()
		if os.Geteuid() == 0 {
			t.Skip("root opens a file whatever its mode")
		}
		path := filepath.Join(home, agyCredentialFileName)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func symlinkFile(data []byte) func(t *testing.T, home string) {
	return func(t *testing.T, home string) {
		t.Helper()
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(home, agyCredentialFileName)); err != nil {
			t.Fatal(err)
		}
	}
}

// Without a fallback file the launch is on the Keychain and asks
// security(1) nothing; with one, a query that gets no answer in time fails
// the decision and names the query and the bound, and the caller's
// cancellation ends it too.
func TestSecurityAgyKeychainQueryIsBounded(t *testing.T) {
	installFakeSecurity(t)
	kc := securityAgyKeychain{}
	home := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_SECURITY_LOG", calls)
	t.Setenv("FAKE_SECURITY_FIND_SLEEP", "30")

	if fileStorage, err := kc.canonicalFileStorage(context.Background(), home, 50*time.Millisecond); err != nil || fileStorage {
		t.Fatalf("without a file = %v, %v; want the Keychain", fileStorage, err)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Fatal("deciding without a fallback file ran security(1)")
	}

	writeFile([]byte("{}"))(t, home)
	start := time.Now()
	_, err := kc.canonicalFileStorage(context.Background(), home, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "query Antigravity Keychain credential") || !strings.Contains(err.Error(), "within 50ms") {
		t.Fatalf("a query with no answer = %v, want it named with its bound", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the bounded query took %s", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := kc.canonicalFileStorage(ctx, home, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("a query after the caller gave up = %v, want its cancellation", err)
	}
}

// A canonical read is one exchange with security(1), whatever the two
// stores hold: the item read, which falls back to the file when there is no
// item.
func TestSecurityAgyKeychainCanonicalReadIsOneExchange(t *testing.T) {
	installFakeSecurity(t)
	kc := securityAgyKeychain{}
	item := []byte(`{"id_token_email":"keychain@example.com","token":{"access_token":"k1","refresh_token":"rk"}}`)
	t.Setenv("FAKE_SECURITY_READ_PAYLOAD", string(encodeGoKeyringPayload(item)))
	for _, exit := range []string{"0", "44"} {
		home := t.TempDir()
		writeFile([]byte(`{"token":{"access_token":"f1"}}`))(t, home)
		calls := filepath.Join(t.TempDir(), "calls")
		t.Setenv("FAKE_SECURITY_LOG", calls)
		t.Setenv("FAKE_SECURITY_FIND_EXIT", exit)
		if _, err := kc.read(home, true); err != nil {
			t.Fatalf("security exit %s: read: %v", exit, err)
		}
		logged, err := os.ReadFile(calls)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(logged), "\n"); n != 1 {
			t.Fatalf("security exit %s: a canonical read ran security(1) %d times:\n%s", exit, n, logged)
		}
	}
}
