package agy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/testutil/mockexec"
)

// recordingAgy is a mock `agy acp` that answers an account probe and writes,
// to the file it returns, the store and home it was pointed at: its argv and
// the variables agy reads them from. An unset variable is recorded as
// "<unset>".
func recordingAgy(t *testing.T) (binary, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "launch.txt")
	binary = mockexec.Write(t, filepath.Join(dir, "mock-agy"), `#!/bin/bash
{
  printf 'args=%s\n' "$*"
  printf 'AGY_ACP_FORCE_FILE_STORAGE=%s\n' "${AGY_ACP_FORCE_FILE_STORAGE-<unset>}"
  printf 'GEMINI_HOME=%s\n' "${GEMINI_HOME-<unset>}"
  printf 'AGY_APP_DATA_DIR=%s\n' "${AGY_APP_DATA_DIR-<unset>}"
} > '`+record+`'
while IFS= read -r line; do
  id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
  if [ -z "$id" ]; then continue; fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentInfo":{"name":"antigravity-mock","version":"1.5.13"},"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"close":{}}}}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"session/new"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"agy-probe"}}\n' "$id"
  else
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  fi
done
`)
	return binary, record
}

// readLaunchRecord parses what recordingAgy wrote.
func readLaunchRecord(t *testing.T, record string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read the mock agy's launch record: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, value, _ := strings.Cut(line, "=")
		out[name] = value
	}
	return out
}

// inheritOtherAgyStore gives this process the variables that point agy at
// another store and home than the account manager's.
func inheritOtherAgyStore(t *testing.T) {
	t.Helper()
	t.Setenv("AGY_ACP_FORCE_FILE_STORAGE", "1")
	t.Setenv("GEMINI_HOME", filepath.Join(t.TempDir(), "other-gemini"))
	t.Setenv("AGY_APP_DATA_DIR", filepath.Join(t.TempDir(), "other-app-data"))
}

func probeWith(t *testing.T, binary string, home Home) {
	t.Helper()
	if _, err := ProbeAccount(context.Background(), ProbeConfig{
		Binary:  binary,
		WorkDir: t.TempDir(),
		Home:    home,
		Reaper:  procutiltest.Reaper(),
	}); err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
}

func wantCleared(t *testing.T, got map[string]string, names ...string) {
	t.Helper()
	for _, name := range names {
		if got[name] != "<unset>" {
			t.Errorf("%s reached the agy process as %q, want it cleared", name, got[name])
		}
	}
}

// The canonical home on the Keychain is where the account manager reads the
// macOS Keychain. Inherited variables must not move it to the fallback file
// or to another home.
func TestACanonicalKeychainLaunchClearsAnInheritedStoreAndHome(t *testing.T) {
	inheritOtherAgyStore(t)
	binary, record := recordingAgy(t)

	probeWith(t, binary, CanonicalHome(false))

	got := readLaunchRecord(t, record)
	wantCleared(t, got, "AGY_ACP_FORCE_FILE_STORAGE", "GEMINI_HOME", "AGY_APP_DATA_DIR")
	if got["args"] != "acp" {
		t.Errorf("canonical agy args = %q, want %q", got["args"], "acp")
	}
}

// The canonical home on file storage gets the flag, and inherited home
// variables are still cleared.
func TestACanonicalFileLaunchSetsFileStorageAndClearsAnInheritedHome(t *testing.T) {
	inheritOtherAgyStore(t)
	binary, record := recordingAgy(t)

	probeWith(t, binary, CanonicalHome(true))

	got := readLaunchRecord(t, record)
	if got["AGY_ACP_FORCE_FILE_STORAGE"] != "1" {
		t.Errorf("AGY_ACP_FORCE_FILE_STORAGE = %q, want 1", got["AGY_ACP_FORCE_FILE_STORAGE"])
	}
	wantCleared(t, got, "GEMINI_HOME", "AGY_APP_DATA_DIR")
	if got["args"] != "acp" {
		t.Errorf("canonical agy args = %q, want %q", got["args"], "acp")
	}
}

// An isolated home names its own directory and is on file storage, whatever
// the process inherited.
func TestAnIsolatedLaunchNamesItsHomeOnFileStorage(t *testing.T) {
	inheritOtherAgyStore(t)
	t.Setenv("AGY_ACP_FORCE_FILE_STORAGE", "")
	binary, record := recordingAgy(t)
	appData := filepath.Join(t.TempDir(), "gemini", "antigravity-cli")
	home, err := IsolatedHome(appData)
	if err != nil {
		t.Fatal(err)
	}

	probeWith(t, binary, home)

	got := readLaunchRecord(t, record)
	if got["AGY_ACP_FORCE_FILE_STORAGE"] != "1" {
		t.Errorf("AGY_ACP_FORCE_FILE_STORAGE = %q, want 1", got["AGY_ACP_FORCE_FILE_STORAGE"])
	}
	wantCleared(t, got, "GEMINI_HOME", "AGY_APP_DATA_DIR")
	want := "acp --gemini-dir " + filepath.Dir(appData) + " --app-data-dir antigravity-cli"
	if got["args"] != want {
		t.Errorf("isolated agy args = %q, want %q", got["args"], want)
	}
}

// No launch runs without a home, and a sign-in never runs on the canonical
// one.
func TestALaunchNeedsAHomeAndASignInAnIsolatedOne(t *testing.T) {
	if _, err := (Home{}).Launch(nil); err == nil {
		t.Fatal("a launch without a home was built")
	}
	if _, err := IsolatedHome(" "); err == nil {
		t.Fatal("an isolated home without a directory was made")
	}
	if _, err := StartLogin(context.Background(), LoginConfig{
		Binary: filepath.Join(t.TempDir(), "agy-never-run"),
		Home:   CanonicalHome(true),
		Reaper: procutiltest.Reaper(),
	}); err == nil || !strings.Contains(err.Error(), "isolated home") {
		t.Fatalf("StartLogin on the canonical home = %v, want a refusal", err)
	}
}
