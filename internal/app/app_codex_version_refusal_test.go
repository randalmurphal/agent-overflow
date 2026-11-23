//go:build unix

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
)

// versionedCodex is a fake app-server whose bytes are the file itself
// (mockexec.WriteFile), so rewriting it gives the binary a new identity the
// way an update does. Each app-server launch appends its pid and arguments
// to launchLog; a `--version` probe prints firstVersion and is not counted. The first
// launch reports firstVersion at initialize and every later launch reports
// laterVersion. With unsubscribeFails, `thread/unsubscribe` answers an
// error, which keeps the thread loaded on the shared app-server and sends
// the next resume to a dedicated process.
type versionedCodex struct {
	path              string
	launchLog         string
	firstVersion      string
	laterVersion      string
	unsubscribeFails  bool
	providerThreadIDs string
}

func (c versionedCodex) write(t *testing.T) {
	t.Helper()
	unsubscribe := `tid=$(/bin/echo "$line" | /usr/bin/grep -o '"threadId":"[^"]*"' | /usr/bin/head -1 | /usr/bin/cut -d'"' -f4)
    printf '{"jsonrpc":"2.0","id":%s,"result":{"status":"unloaded"}}\n' "$id"
    if [ -n "$tid" ]; then
      printf '{"jsonrpc":"2.0","method":"thread/closed","params":{"threadId":"%s"}}\n' "$tid"
    fi`
	if c.unsubscribeFails {
		unsubscribe = `printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"unsubscribe failed"}}\n' "$id"`
	}
	script := `#!/bin/bash
if [ "$1" = "--version" ]; then
  /bin/echo "$*" >> '` + c.launchLog + `.probes'
  /bin/echo 'codex-cli ` + c.firstVersion + `'
  exit 0
fi
/bin/echo "$$ $*" >> '` + c.launchLog + `'
launches=$(/usr/bin/wc -l < '` + c.launchLog + `')
version='` + c.laterVersion + `'
if [ "$launches" -le 1 ]; then version='` + c.firstVersion + `'; fi
while IFS= read -r line; do
  id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
  if [ -z "$id" ]; then continue; fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"userAgent":"codex_cli_rs/%s (fake)"}}\n' "$id" "$version"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"config/read"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"config":{}}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"` + c.providerThreadIDs + `"}}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/resume"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"` + c.providerThreadIDs + `"}}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/unsubscribe"'; then
    ` + unsubscribe + `
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"skills/list"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[]}}\n' "$id"
  else
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  fi
done
`
	mockexec.WriteFile(t, c.path, []byte(script), 0o755)
}

func newVersionedCodex(t *testing.T, version string) versionedCodex {
	t.Helper()
	dir := t.TempDir()
	return versionedCodex{
		path:              filepath.Join(dir, "codex"),
		launchLog:         filepath.Join(dir, "launches.log"),
		firstVersion:      version,
		laterVersion:      version,
		providerThreadIDs: "codex-version-thread",
	}
}

func (c versionedCodex) launches(t *testing.T) []int {
	t.Helper()
	data, err := os.ReadFile(c.launchLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read launch log: %v", err)
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		pid, err := strconv.Atoi(strings.Fields(line)[0])
		if err != nil {
			t.Fatalf("launch log line %q: %v", line, err)
		}
		pids = append(pids, pid)
	}
	return pids
}

func wantCodexVersionMessage(path, version string) string {
	return "Codex v" + version + " (" + path + ") is installed; Agent Overflow needs v" +
		provider.MinimumCodexCLIVersion + " or newer. Update Codex."
}

// requireCodexVersionRefusal checks the error a caller gets back: the
// typed refusal, its text, and the public message the transport answers a
// remote caller with, which carries no wrapper prefixes.
func requireCodexVersionRefusal(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("the start was accepted; want the Codex version refusal")
	}
	if !errors.Is(err, provider.ErrCodexVersionUnsupported) {
		t.Fatalf("error = %v, want ErrCodexVersionUnsupported", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err, want)
	}
	code, message, ok := errorsx.PublicDetails(err)
	if !ok || code != provider.CodexVersionUnsupportedCode || message != want {
		t.Fatalf("public details = (%q, %q, %v), want (%q, %q, true)",
			code, message, ok, provider.CodexVersionUnsupportedCode, want)
	}
}

func setCodexBinary(t *testing.T, app *App, path string) {
	t.Helper()
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": path}); err != nil {
		t.Fatal(err)
	}
}

// A Codex below the minimum is refused at initialize, leaves nothing
// behind, and is not launched again for later starts while the file is
// unchanged. An update gives the file a new identity, and the next start
// launches it and succeeds without a restart of the app.
func TestCodexVersionRefusalIsFinalUntilTheBinaryChanges(t *testing.T) {
	t.Parallel()
	app, bus := setupE2EApp(t)
	fake := newVersionedCodex(t, "0.153.0")
	fake.write(t)
	setCodexBinary(t, app, fake.path)
	thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatal(err)
	}
	want := wantCodexVersionMessage(fake.path, "0.153.0")

	requireCodexVersionRefusal(t, app.StartSession(thread.ID), want)
	if _, ok := app.sessionRuntime.Get(thread.ID); ok {
		t.Fatal("a refused start left a session registered")
	}
	if n := app.sessionManager().runtime.Connections().ActiveCount(); n != 0 {
		t.Fatalf("pool active connections = %d after a refused dial, want 0", n)
	}
	if n := len(fake.launches(t)); n != 1 {
		t.Fatalf("launches = %d, want 1", n)
	}

	// Every later start against the same file is answered with the same
	// refusal and launches nothing.
	for range 3 {
		requireCodexVersionRefusal(t, app.StartSession(thread.ID), want)
	}
	if n := len(fake.launches(t)); n != 1 {
		t.Fatalf("launches = %d after repeated starts, want the refused binary launched once", n)
	}

	// The update: same path, new bytes.
	fake.firstVersion, fake.laterVersion = provider.MinimumCodexCLIVersion, provider.MinimumCodexCLIVersion
	fake.write(t)
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession after the update: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	if _, ok := app.sessionRuntime.Get(thread.ID); !ok {
		t.Fatal("no session after the update")
	}
	if n := len(fake.launches(t)); n != 2 {
		t.Fatalf("launches = %d, want the updated binary launched once more", n)
	}
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatal(err)
	}
}

// A send that has to start the session puts the refusal in the thread the
// person sent from, and returns it to the caller.
func TestCodexVersionRefusalReachesTheThreadOnSend(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	fake := newVersionedCodex(t, "0.153.0")
	fake.write(t)
	setCodexBinary(t, app, fake.path)
	thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatal(err)
	}
	want := wantCodexVersionMessage(fake.path, "0.153.0")

	requireCodexVersionRefusal(t, app.SendMessage(context.Background(), thread.ID, "hello", nil), want)
	waitProviderEvents(t, app, thread.ID)
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		// Both the send and the start name the failed step; the refusal's
		// own message follows them.
		if item.Kind == "error" && item.Summary == "Failed to send: start session: start session: "+want {
			found = true
		}
	}
	if !found {
		t.Fatalf("no error row reads %q: %+v", "Failed to send: start session: start session: "+want, items)
	}
	if _, ok := app.sessionRuntime.Get(thread.ID); ok {
		t.Fatal("a refused send left a session registered")
	}
}

// The shared app-server could not unload the thread, so the start falls
// back to a dedicated process. That process is a launch of its own and is
// refused the same way; the start fails with the refusal and registers no
// session.
func TestCodexVersionRefusalOnTheDedicatedFallback(t *testing.T) {
	t.Parallel()
	app, bus := setupE2EApp(t)
	fake := newVersionedCodex(t, provider.MinimumCodexCLIVersion)
	fake.laterVersion = "0.153.0"
	fake.unsubscribeFails = true
	fake.write(t)
	setCodexBinary(t, app, fake.path)
	thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatal(err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("first StartSession: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	// The stop cannot unload the thread, so the shared app-server still
	// holds it and the next resume needs a dedicated process.
	_ = app.StopSession(thread.ID)

	err = app.StartSession(thread.ID)
	requireCodexVersionRefusal(t, err, wantCodexVersionMessage(fake.path, "0.153.0"))
	if _, ok := app.sessionRuntime.Get(thread.ID); ok {
		t.Fatal("a refused fallback left a session registered")
	}
	if n := len(fake.launches(t)); n != 2 {
		t.Fatalf("launches = %d, want the shared app-server and one dedicated launch", n)
	}
	// The refusal came from the dedicated dial: the shared app-server is
	// still the pool's, idle.
	if n := app.sessionManager().runtime.Connections().ActiveCount(); n != 1 {
		t.Fatalf("pool active connections = %d, want the shared app-server kept", n)
	}
}

// A session dies after Codex was replaced by a version below the minimum.
// The automatic reconnect makes its one attempt, which is refused, and the
// refusal is written in the thread. Nothing launches the binary again.
func TestCodexVersionRefusalOnAutoReconnect(t *testing.T) {
	t.Parallel()
	app, bus := setupE2EApp(t)
	fake := newVersionedCodex(t, provider.MinimumCodexCLIVersion)
	fake.write(t)
	setCodexBinary(t, app, fake.path)
	thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	waitProviderEvents(t, app, thread.ID)
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SessionRef == "" {
		t.Fatal("the session published no resume cursor; auto-reconnect would not run")
	}

	fake.firstVersion, fake.laterVersion = "0.153.0", "0.153.0"
	fake.write(t)
	pids := fake.launches(t)
	if len(pids) != 1 {
		t.Fatalf("launches = %d before the death, want 1", len(pids))
	}
	if err := syscall.Kill(pids[0], syscall.SIGKILL); err != nil {
		t.Fatalf("kill the app-server: %v", err)
	}

	want := "session could not be resumed automatically: start session: " + wantCodexVersionMessage(fake.path, "0.153.0")
	deadline := time.Now().Add(testutil.WaitBudget(t))
	for {
		items, err := app.store.ListItems(thread.ID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range items {
			if item.Kind == "error" && item.Summary == want {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no error row carries %q: %+v", want, items)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitProviderEvents(t, app, thread.ID)
	if n := len(fake.launches(t)); n != 2 {
		t.Fatalf("launches = %d, want the one reconnect attempt", n)
	}
	if _, ok := app.sessionRuntime.Get(thread.ID); ok {
		t.Fatal("a refused reconnect left a session registered")
	}
	// A start after the reconnect is answered from the refusal without a
	// launch.
	requireCodexVersionRefusal(t, app.StartSession(thread.ID), wantCodexVersionMessage(fake.path, "0.153.0"))
	if n := len(fake.launches(t)); n != 2 {
		t.Fatalf("launches = %d after a later start, want 2", n)
	}
}
