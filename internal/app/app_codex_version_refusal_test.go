//go:build unix

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/workflow/engine"
)

// versionedCodex is a fake app-server whose bytes are the file itself
// (mockexec.WriteFile), so rewriting it gives the binary a new identity the
// way an update does. Each app-server launch appends its pid and arguments
// to launchLog; a `--version` probe prints firstVersion and is not counted. The first
// launch reports firstVersion at initialize and every later launch reports
// laterVersion. `model/list` offers gpt-5.5. With unsubscribeFails, `thread/unsubscribe` answers an
// error, which keeps the thread loaded on the shared app-server and sends
// the next resume to a dedicated process.
type versionedCodex struct {
	path              string
	launchLog         string
	firstVersion      string
	laterVersion      string
	unsubscribeFails  bool
	providerThreadIDs string
	// selectVar, when set, makes the fake a launcher that runs
	// selectedVersion whenever that variable is "1" in its environment, as
	// a version manager picks a build from its environment.
	selectVar       string
	selectedVersion string
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
  /bin/echo 'codex-cli ` + c.firstVersion + `'
  exit 0
fi
/bin/echo "$$ $*" >> '` + c.launchLog + `'
launches=$(/usr/bin/wc -l < '` + c.launchLog + `')
version='` + c.laterVersion + `'
if [ "$launches" -le 1 ]; then version='` + c.firstVersion + `'; fi
` + c.selectScript() + `
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
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"model/list"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[{"model":"gpt-5.5","displayName":"GPT-5.5","hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"high","description":"High"}],"defaultReasoningEffort":"high"}],"nextCursor":null}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"skills/list"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[]}}\n' "$id"
  else
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  fi
done
`
	mockexec.WriteFile(t, c.path, []byte(script), 0o755)
}

// selectScript is the line that lets the environment pick the build.
func (c versionedCodex) selectScript() string {
	if c.selectVar == "" {
		return ""
	}
	return `if [ "${` + c.selectVar + `}" = 1 ]; then version='` + c.selectedVersion + `'; fi`
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
	// A launch's shell creates the log before it writes its line, so a read
	// can find the file empty or its last line not yet ended.
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if !strings.HasSuffix(line, "\n") {
			continue
		}
		pid, err := strconv.Atoi(strings.Fields(line)[0])
		if err != nil {
			t.Fatalf("launch log line %q: %v", line, err)
		}
		pids = append(pids, pid)
	}
	return pids
}

// requireLaunches checks how many app-server launches the fake has seen.
func requireLaunches(t *testing.T, fake versionedCodex, want int) {
	t.Helper()
	if n := len(fake.launches(t)); n != want {
		t.Fatalf("app-server launches = %d, want %d", n, want)
	}
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

// A Codex below the minimum is refused at initialize and leaves nothing
// behind. Each start is one dial, with no retry inside it. After an update the next start
// launches the new binary and succeeds without a restart of the app.
func TestCodexVersionRefusalEachStartDialsOnce(t *testing.T) {
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
	requireLaunches(t, fake, 1)

	// Every later start dials again, once, and is refused the same way.
	for range 3 {
		requireCodexVersionRefusal(t, app.StartSession(thread.ID), want)
	}
	requireLaunches(t, fake, 4)

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
	requireLaunches(t, fake, 5)
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
		if item.Kind == "error" && item.Summary == "Failed to send: start session: "+want {
			found = true
		}
	}
	if !found {
		t.Fatalf("no error row reads %q: %+v", "Failed to send: start session: "+want, items)
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
// refusal is written in the thread. Nothing retries it.
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
	// A later start is a new attempt: one dial, refused the same way.
	requireCodexVersionRefusal(t, app.StartSession(thread.ID), wantCodexVersionMessage(fake.path, "0.153.0"))
	if n := len(fake.launches(t)); n != 3 {
		t.Fatalf("launches = %d after a later start, want 3", n)
	}
}

// useCodexCatalog gives f's app the real Codex model catalog, which reads
// `model/list` from the configured binary, over the isolation's disabled one.
func useCodexCatalog(f *requestFixture) {
	f.app.providerDiscoveryCaches.CodexModels = codexmodels.NewWith(time.Minute, nil, nil)
}

// spawnCodexAnswer calls thread_spawn for a Codex thread the way an agent
// does and returns what the agent reads: the refusal's text.
func spawnCodexAnswer(t *testing.T, f *requestFixture) string {
	t.Helper()
	answer, err := f.app.threadToolsServer().Call(t.Context(), f.callerIdentity(), "thread_spawn",
		json.RawMessage(`{"prompt":"work","provider":"codex"}`))
	if err == nil {
		t.Fatalf("thread_spawn on a refused Codex answered %+v, want a refusal", answer)
	}
	_, message, ok := errorsx.PublicDetails(err)
	if !ok {
		t.Fatalf("thread_spawn refused with %v, which has no message for the agent", err)
	}
	return message
}

// An agent spawning a Codex thread while the model catalog is cold reaches
// Codex first through the catalog's handshake. The refusal there is the
// spawn's answer, not "Codex does not run on this computer", and no thread
// is created.
func TestCodexVersionRefusalAnswersAColdCatalogSpawn(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	useCodexCatalog(f)
	fake := newVersionedCodex(t, "0.153.4")
	fake.write(t)
	if _, err := f.app.settings.Update(map[string]any{"codexBinaryPath": fake.path}); err != nil {
		t.Fatal(err)
	}
	before, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}

	got := spawnCodexAnswer(t, f)

	if want := wantCodexVersionMessage(fake.path, "0.153.4"); !strings.Contains(got, want) {
		t.Fatalf("thread_spawn answered %q, want it to carry %q", got, want)
	}
	if strings.Contains(got, "does not run") {
		t.Fatalf("thread_spawn answered %q, which hides the refusal", got)
	}
	after, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("threads = %d after a refused spawn, want %d", len(after), len(before))
	}
}

// A launcher that picks the supported build only under the environment AO
// gives Codex: the catalog starts it the way a session would, so a cold
// catalog lists the build a session runs and the spawn is not refused.
func TestCodexCatalogStartsTheBuildASessionRuns(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	useCodexCatalog(f)
	fake := newVersionedCodex(t, "0.153.4")
	fake.selectVar = "CODEX_TEST_SELECT"
	fake.selectedVersion = provider.MinimumCodexCLIVersion
	fake.write(t)
	if _, err := f.app.settings.Update(map[string]any{"codexBinaryPath": fake.path}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.providerDiscoveryService().SetProviderCustomEnvVar(
		string(provider.Codex), fake.selectVar, "1", false,
	); err != nil {
		t.Fatal(err)
	}

	answer, err := f.app.threadToolsServer().Call(t.Context(), f.callerIdentity(), "thread_spawn",
		json.RawMessage(`{"prompt":"work","provider":"codex"}`))
	if err != nil {
		t.Fatalf("thread_spawn on a launcher that runs %s under AO's environment was refused: %v",
			provider.MinimumCodexCLIVersion, err)
	}
	if encoded, _ := json.Marshal(answer); !strings.Contains(string(encoded), `"thread_id"`) {
		t.Fatalf("thread_spawn answered %s, want the created thread", encoded)
	}
	if n := len(fake.launches(t)); n < 1 {
		t.Fatalf("app-server launches = %d, want the catalog's", n)
	}
}

// With the catalog read from a supported Codex, a spawn creates the thread
// and its start meets the refusal. The answer carries the refusal and says
// the thread was created and its prompt did not run, so the agent sends the
// prompt there instead of spawning again.
func TestCodexVersionRefusalAnswersASpawnWhoseStartIsRefused(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	useCodexCatalog(f)
	fake := newVersionedCodex(t, provider.MinimumCodexCLIVersion)
	fake.laterVersion = "0.153.4"
	fake.write(t)
	if _, err := f.app.settings.Update(map[string]any{"codexBinaryPath": fake.path}); err != nil {
		t.Fatal(err)
	}

	got := spawnCodexAnswer(t, f)

	if want := wantCodexVersionMessage(fake.path, "0.153.4"); !strings.Contains(got, want) {
		t.Fatalf("thread_spawn answered %q, want it to carry %q", got, want)
	}
	if !strings.Contains(got, "was created for it and its prompt did not run") {
		t.Fatalf("thread_spawn answered %q, want it to name the created thread", got)
	}
}

// A paired send to an idle Codex thread on a computer whose Codex is below
// the minimum is refused there when its session starts. That computer
// answers with a thread refusal that carries the message, so the sender
// gets the message once and does not dispatch the send again.
func TestCodexVersionRefusalReachesAPairedSenderOnce(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := newFaultyReachPair(t)
	fake := newVersionedCodex(t, "0.153.4")
	fake.write(t)
	if _, err := pair.dest.settings.Update(map[string]any{"codexBinaryPath": fake.path}); err != nil {
		t.Fatal(err)
	}
	target, err := createTestThread(t, pair.dest, string(provider.Codex), t.TempDir(), "gpt-5.5", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create the destination's Codex thread: %v", err)
	}

	_, err = pair.callRaw(t, "thread_send", `{"thread_id":"`+target.ID+`","message":"hello"}`)

	_, message, ok := errorsx.PublicDetails(err)
	if want := wantCodexVersionMessage(fake.path, "0.153.4"); !ok || !strings.Contains(message, want) {
		t.Fatalf("thread_send answered %v, want it to carry %q", err, want)
	}
	if n := faults.entered.Load(); n != 1 {
		t.Fatalf("the destination received the send %d times, want once", n)
	}
	requireLaunches(t, fake, 1)
}

// A workflow phase on a Codex below the minimum is refused when its session
// starts. The run parks for a person with the refusal as its cause, and the
// phase is not tried again: the binary was dialed once.
func TestCodexVersionRefusalParksAWorkflowPhase(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	workflowDir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(workflowDir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := `id: codex-refused
name: Codex refused
inputs:
  goal:
    schema:
      type: string
phases:
  - id: execute
    driver: agent
    provider: codex
    model: gpt-5.5
    prompt: execute.md
    inputs:
      goal:
        schema:
          type: string
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
cleanup: manual
`
	if err := os.WriteFile(filepath.Join(workflowDir, "codex-refused.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, "execute.md"), []byte("Execute {{goal}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newVersionedCodex(t, "0.153.4")
	fake.write(t)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": fake.path}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app.workflowApplication().Engine() != nil {
			_ = app.workflowApplication().Engine().Close()
		}
	})
	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	item, err := app.WorkflowStartRun(projectRow.ID, "codex-refused", "shared", "refused",
		json.RawMessage(`{"goal":"refused"}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}

	var parked store.WorkItem
	waitUntil(t, testutil.WaitBudget(t), func() bool {
		row, err := app.store.GetWorkItem(item.ID)
		parked = row
		return err == nil && row.State == string(engine.StateNeedsHuman)
	})
	phases, err := app.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := wantCodexVersionMessage(fake.path, "0.153.4")
	if len(phases) != 1 || !strings.Contains(phases[0].ParkCause, want) {
		t.Fatalf("parked %s with phases %+v, want one attempt whose cause carries %q", parked.Reason, phases, want)
	}
	requireLaunches(t, fake, 1)
}

// A loop's wake to an idle Codex thread starts its session, and Codex below
// the minimum refuses it. Each refused start writes its error row, which
// names the start once and carries the refusal, and the person is notified
// once for the run of failures. A second loop's wake makes the second start:
// a loop whose wake is still queued skips its next fire.
func TestCodexVersionRefusalOfAWakeStartNotifiesOnce(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	recorder := &recordingNotificationSender{}
	a.osNotifications = recorder
	a.storeIdentity.Store(&store.Identity{BackendID: "backend-under-test"})
	fake := newVersionedCodex(t, "0.153.4")
	fake.write(t)
	if _, err := a.settings.Update(map[string]any{"codexBinaryPath": fake.path}); err != nil {
		t.Fatal(err)
	}
	thread, err := createTestThread(t, a, string(provider.Codex), t.TempDir(), "gpt-5.5", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })

	refusal := wantCodexVersionMessage(fake.path, "0.153.4")
	var want []string
	for fire, label := range []string{"board", "news"} {
		loop := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: label,
			Note: "Check the " + label + ".", IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
		now := time.Now()
		if err := a.fireAgentWake(loop, now, agentWakeFire{at: now.UnixMilli()}); err != nil {
			t.Fatalf("fire %s: %v", label, err)
		}
		waitUntil(t, testutil.WaitBudget(t), func() bool { return len(fake.launches(t)) == fire+1 })
		a.threadRequestsWG.Wait()
		a.waitAgentNoticeWork()
		a.notifications.queue.Wait()
		want = append(want, `The loop "`+label+`" woke this thread and its message is queued, but the agent could not start: `+refusal)
		if rows := threadErrorRows(t, a.store, thread.ID, ""); !slices.Equal(rows, want) {
			t.Fatalf("after the %s wake the error rows are %q, want %q", label, rows, want)
		}
	}
	if sends := recorder.snapshot(); len(sends) != 1 || sends[0].ID != "session-start:"+thread.ID {
		t.Fatalf("notifications %#v, want one for the failed start", sends)
	}
	requireLaunches(t, fake, 2)
}
