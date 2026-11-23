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

// A canonical probe that is not told to use file storage runs where the
// account manager reads the macOS Keychain. Inherited variables must not move
// it to the fallback file or to another home.
func TestACanonicalKeychainLaunchClearsAnInheritedStoreAndHome(t *testing.T) {
	inheritOtherAgyStore(t)
	binary, record := recordingAgy(t)

	if _, err := ProbeAccount(context.Background(), ProbeConfig{
		Binary:  binary,
		WorkDir: t.TempDir(),
		Reaper:  procutiltest.Reaper(),
	}); err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}

	got := readLaunchRecord(t, record)
	for _, name := range []string{"AGY_ACP_FORCE_FILE_STORAGE", "GEMINI_HOME", "AGY_APP_DATA_DIR"} {
		if got[name] != "<unset>" {
			t.Errorf("%s reached the canonical agy process as %q, want it cleared", name, got[name])
		}
	}
	if got["args"] != "acp" {
		t.Errorf("canonical agy args = %q, want %q", got["args"], "acp")
	}
}

// A canonical probe told to use file storage gets the flag, and inherited
// home variables are still cleared.
func TestACanonicalFileLaunchSetsFileStorageAndClearsAnInheritedHome(t *testing.T) {
	inheritOtherAgyStore(t)
	binary, record := recordingAgy(t)

	if _, err := ProbeAccount(context.Background(), ProbeConfig{
		Binary:        binary,
		WorkDir:       t.TempDir(),
		Reaper:        procutiltest.Reaper(),
		ForceFileAuth: true,
	}); err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}

	got := readLaunchRecord(t, record)
	if got["AGY_ACP_FORCE_FILE_STORAGE"] != "1" {
		t.Errorf("AGY_ACP_FORCE_FILE_STORAGE = %q, want 1", got["AGY_ACP_FORCE_FILE_STORAGE"])
	}
	for _, name := range []string{"GEMINI_HOME", "AGY_APP_DATA_DIR"} {
		if got[name] != "<unset>" {
			t.Errorf("%s reached the canonical agy process as %q, want it cleared", name, got[name])
		}
	}
}
