package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// A thread's agy session runs on the shared ACP server the pool starts, and
// that server reads the login store the account manager reads, not the one
// the environment Agent Overflow inherited names. This app's manager keeps
// the canonical login in a file, so the server is on file storage on agy's
// default home. (The fixture refuses an inherited home variable outright, so
// the home half is proven by the agy and provideraccountapp tests.)
//
// Serial: it sets the process environment.
func TestAgySessionServerRunsOnTheManagersStoreAndHome(t *testing.T) {
	t.Setenv("AGY_ACP_FORCE_FILE_STORAGE", "")
	app, _ := setupE2EApp(t)
	attachFileAccountStoresForTest(t, app)
	record := filepath.Join(t.TempDir(), "launch.txt")
	binary := writePoolAgyBinary(t, "", record)
	if _, err := app.settings.Update(map[string]any{"agyBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	thread, err := createTestThread(t, app, string(provider.Agy), t.TempDir(), "gemini-3.1-pro-high", "chat")
	if err != nil {
		t.Fatal(err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read the mock agy's launch record: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, value, _ := strings.Cut(line, "=")
		got[name] = value
	}
	if got["AGY_ACP_FORCE_FILE_STORAGE"] != "1" || got["GEMINI_HOME"] != "<unset>" || got["AGY_APP_DATA_DIR"] != "<unset>" || got["args"] != "acp" {
		t.Fatalf("session server launched with %v, want file storage on agy's default home", got)
	}
}
