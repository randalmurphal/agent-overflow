package agy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
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
		Home:    home.Source(),
		Reaper:  procutiltest.Reaper(),
	}); err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
}

func canonical(t *testing.T, appDataDir string, fileStorage bool) Home {
	t.Helper()
	home, err := CanonicalHome(appDataDir, fileStorage)
	if err != nil {
		t.Fatal(err)
	}
	return home
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

	probeWith(t, binary, canonical(t, "", false))

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

	probeWith(t, binary, canonical(t, "", true))

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
		Home:   canonical(t, "", true),
		Reaper: procutiltest.Reaper(),
	}); err == nil || !strings.Contains(err.Error(), "isolated home") {
		t.Fatalf("StartLogin on the canonical home = %v, want a refusal", err)
	}
}

// HOME is reserved: a value in the custom environment never reaches agy, and
// the process HOME, which agy's default home lies under, is never cleared.
func TestALaunchKeepsTheProcessHome(t *testing.T) {
	for _, home := range []Home{canonical(t, "", true), canonical(t, "", false)} {
		launch, err := home.Launch(map[string]string{"HOME": "/tmp/elsewhere"})
		if err != nil {
			t.Fatal(err)
		}
		if value, set := launch.Env["HOME"]; set {
			t.Errorf("launch sets HOME=%q, want the process HOME", value)
		}
		if slices.Contains(launch.Unset, "HOME") {
			t.Errorf("launch clears HOME: %v", launch.Unset)
		}
	}
}

// A canonical home that is not agy's default under the process HOME names
// its directory and is still the canonical one; only the file store may name
// one.
func TestACanonicalHomeOutsideTheProcessHomeNamesItsDirectory(t *testing.T) {
	inheritOtherAgyStore(t)
	binary, record := recordingAgy(t)
	appData := filepath.Join(t.TempDir(), "gemini", "antigravity-cli")
	home := canonical(t, appData, true)
	if home.Isolated() {
		t.Fatal("a canonical home with a directory reads as isolated")
	}

	probeWith(t, binary, home)

	got := readLaunchRecord(t, record)
	want := "acp --gemini-dir " + filepath.Dir(appData) + " --app-data-dir antigravity-cli"
	if got["args"] != want {
		t.Errorf("canonical agy args = %q, want %q", got["args"], want)
	}
	if got["AGY_ACP_FORCE_FILE_STORAGE"] != "1" {
		t.Errorf("AGY_ACP_FORCE_FILE_STORAGE = %q, want 1", got["AGY_ACP_FORCE_FILE_STORAGE"])
	}
	if _, err := CanonicalHome(appData, false); err == nil {
		t.Fatal("a canonical home named its directory on the Keychain")
	}
}

// A launcher decides its home inside its own time limit: the source sees
// the probe's deadline, and its error stops the launch.
func TestALauncherDecidesItsHomeInsideItsTimeLimit(t *testing.T) {
	binary, record := recordingAgy(t)
	var sawDeadline bool
	_, err := ProbeAccount(context.Background(), ProbeConfig{
		Binary:  binary,
		WorkDir: t.TempDir(),
		Home: func(ctx context.Context) (Home, error) {
			_, sawDeadline = ctx.Deadline()
			return Home{}, errors.New("store query failed")
		},
		Reaper: procutiltest.Reaper(),
	})
	if err == nil || !strings.Contains(err.Error(), "store query failed") {
		t.Fatalf("ProbeAccount = %v, want the home's error", err)
	}
	if !sawDeadline {
		t.Error("the probe decided its home outside its time limit")
	}
	if _, statErr := os.Stat(record); statErr == nil {
		t.Error("agy started although its home could not be decided")
	}
}

// A name a launch sets or clears on purpose is reserved, so a custom
// environment cannot ask for a value the launch would discard.
func TestTheNamesALaunchSetsAreReserved(t *testing.T) {
	reserved := provider.ReservedEnvNames(string(provider.Agy))
	for _, name := range []string{forceFileStorageEnv, geminiHomeEnv, appDataDirEnv, sshConnectionEnv, "HOME"} {
		if !slices.Contains(reserved, name) {
			t.Errorf("%s is not reserved for Antigravity: %v", name, reserved)
		}
	}
}

// `agy mcp list` runs on the store its home names, inside the fetch's time
// limit, and refuses a home that names its directory, which only `agy acp`
// is known to take.
func TestMCPListRunsOnItsHomeAndOnlyAgysDefault(t *testing.T) {
	inheritOtherAgyStore(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "launch.txt")
	binary := mockexec.Write(t, filepath.Join(dir, "mock-agy"), `#!/bin/bash
{
  printf 'args=%s\n' "$*"
  printf 'AGY_ACP_FORCE_FILE_STORAGE=%s\n' "${AGY_ACP_FORCE_FILE_STORAGE-<unset>}"
  printf 'GEMINI_HOME=%s\n' "${GEMINI_HOME-<unset>}"
  printf 'AGY_APP_DATA_DIR=%s\n' "${AGY_APP_DATA_DIR-<unset>}"
} > '`+record+`'
echo 'No MCP servers configured.'
`)
	var sawDeadline bool
	fetcher := &MCPStatusFetcher{Binary: binary, Home: func(ctx context.Context) (Home, error) {
		_, sawDeadline = ctx.Deadline()
		return CanonicalHome("", false)
	}}
	if _, err := fetcher.Fetch(context.Background(), ""); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got := readLaunchRecord(t, record)
	wantCleared(t, got, "AGY_ACP_FORCE_FILE_STORAGE", "GEMINI_HOME", "AGY_APP_DATA_DIR")
	if got["args"] != "mcp list" {
		t.Errorf("agy mcp list args = %q", got["args"])
	}
	if !sawDeadline {
		t.Error("the fetch decided its home outside its time limit")
	}

	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	named := canonical(t, filepath.Join(t.TempDir(), "gemini", "antigravity-cli"), true)
	fetcher.Home = named.Source()
	if _, err := fetcher.Fetch(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "default home") {
		t.Fatalf("Fetch on a home naming its directory = %v, want a refusal", err)
	}
	if _, err := os.Stat(record); err == nil {
		t.Error("agy mcp list ran on a home it cannot be pointed at")
	}
}

// A home's directory is absolute: a relative one is refused, by name, for
// the canonical home and an isolated one alike.
func TestAHomeDirectoryIsAbsolute(t *testing.T) {
	if _, err := CanonicalHome("relative/.gemini/antigravity-cli", true); err == nil || !strings.Contains(err.Error(), `"relative/.gemini/antigravity-cli" is not an absolute path`) {
		t.Fatalf("CanonicalHome on a relative directory = %v, want a refusal naming it", err)
	}
	if _, err := IsolatedHome("relative/antigravity-cli"); err == nil || !strings.Contains(err.Error(), "is not an absolute path") {
		t.Fatalf("IsolatedHome on a relative directory = %v, want a refusal", err)
	}
}

// SSH_CONNECTION in a custom environment never reaches agy: a file-store
// launch sets its own, and a Keychain launch drops it and leaves an inherited
// one alone.
func TestALaunchOwnsSSHConnection(t *testing.T) {
	custom := map[string]string{sshConnectionEnv: "10.0.0.1 22 10.0.0.2 22"}
	keychain, err := canonical(t, "", false).Launch(custom)
	if err != nil {
		t.Fatal(err)
	}
	if value, set := keychain.Env[sshConnectionEnv]; set {
		t.Errorf("a Keychain launch sets SSH_CONNECTION=%q from the custom environment", value)
	}
	if slices.Contains(keychain.Unset, sshConnectionEnv) {
		t.Errorf("a Keychain launch clears the inherited SSH_CONNECTION: %v", keychain.Unset)
	}
	file, err := canonical(t, "", true).Launch(custom)
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Env[sshConnectionEnv]; got != "127.0.0.1 0 127.0.0.1 0" {
		t.Errorf("a file-store launch sets SSH_CONNECTION=%q, want its own", got)
	}
}
