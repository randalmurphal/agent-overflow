package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

// Assistants as records (docs/specs/standing-agents.md, "Standing agent"):
// creation, the opening message, name addressing, New Thread, Same
// Assistant and its automatic run when the provider cleaned up a session.

// newAssistantFixture is a request fixture whose queue dispatches, as
// production's does: the app-written opening of an assistant's thread is a
// queued message.
func newAssistantFixture(t *testing.T) *requestFixture {
	t.Helper()
	f := newRequestFixture(t)
	f.app.configureTriageQueueCallbacks()
	return f
}

// mockClaudeRecording installs a provider that records every line it is
// written and answers each user message with reply. A message the app
// queues for a thread reaches the transcript only by the provider's echo,
// which the mock does not send, so tests of queued messages read what the
// provider was written.
func (f *requestFixture) mockClaudeRecording(t *testing.T, reply string) (stdinLog string) {
	t.Helper()
	stdinLog = filepath.Join(t.TempDir(), "stdin.log")
	answer := []string{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":` + quoteJSON(reply) + `}]}}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	}
	var script strings.Builder
	script.WriteString("#!/bin/sh\nset -u\nwhile IFS= read -r line; do\n")
	script.WriteString("  printf '%s\\n' \"$line\" >> " + shellQuoteForTest(stdinLog) + "\n")
	script.WriteString("  case \"$line\" in\n    *'\"type\":\"user\"'*)\n")
	for _, wire := range answer {
		script.WriteString("      printf '%s\\n' " + shellQuoteForTest(wire) + "\n")
	}
	script.WriteString("      ;;\n  esac\ndone\n")
	binary := filepath.Join(t.TempDir(), "claude-recording.sh")
	mockexec.Write(t, binary, script.String())
	if _, err := f.app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	return stdinLog
}

// awaitSent waits until the provider was written a user message matching
// match, and returns its text.
func awaitSent(t *testing.T, stdinLog string, match func(string) bool) string {
	t.Helper()
	var sent []string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		sent = sent[:0]
		for _, envelope := range recordedUserEnvelopes(t, stdinLog) {
			text, _ := envelopeText(t, envelope)
			sent = append(sent, text)
			if match(text) {
				return text
			}
		}
	}
	t.Fatalf("no message the provider was written matches; it was written:\n%s", strings.Join(sent, "\n----\n"))
	return ""
}

// appMessageMarks are the marks the provider receives around a message the
// app writes itself (usermessage.WireText).
const (
	appMessageOpening = "[Agent Overflow message. The user did not write it.]\n"
	appMessageClosing = "\n[End of Agent Overflow message]"
)

// awaitAppMessage waits until the provider was written a message the app
// wrote itself, between its marks, whose body matches match, and returns
// the body.
func awaitAppMessage(t *testing.T, stdinLog string, match func(string) bool) string {
	t.Helper()
	text := awaitSent(t, stdinLog, func(text string) bool {
		body, ok := strings.CutPrefix(text, appMessageOpening)
		body, closed := strings.CutSuffix(body, appMessageClosing)
		return ok && closed && match(body)
	})
	return strings.TrimSuffix(strings.TrimPrefix(text, appMessageOpening), appMessageClosing)
}

// awaitFirstUserText waits for threadID's first user message and returns it.
func awaitFirstUserText(t *testing.T, a *App, threadID string) string {
	t.Helper()
	var text string
	waitUntil(t, 20*time.Second, func() bool {
		items, err := a.store.ListItems(threadID)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.Kind == "user_text" {
				text = item.Summary
				return true
			}
		}
		return false
	})
	return text
}

// awaitCurrentThread waits until the assistant whose thread was threadID
// has moved on, and returns its current thread.
func awaitCurrentThread(t *testing.T, a *App, threadID string) string {
	t.Helper()
	var current string
	waitUntil(t, 20*time.Second, func() bool {
		next, found, err := a.store.AssistantCurrentThreadOf(threadID)
		if err != nil {
			t.Fatal(err)
		}
		current = next
		return found
	})
	return current
}

// New Assistant makes the thread titled with the name, the record and its
// home at once, and starts the thread with the opening message; a name an
// assistant here holds is refused before anything is made.
func TestCreateAssistantMakesTheRecordHomeAndOpening(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	sent := f.mockClaudeRecording(t, "I am Ops. What am I responsible for?")

	thread, err := f.app.CreateAssistant(t.Context(), CreateAssistantOptions{ProjectIDs: []string{f.caller.ProjectID}, Name: " Ops "})
	if err != nil {
		t.Fatalf("CreateAssistant: %v", err)
	}
	if thread.Title != "Ops" || thread.Mode != threadmode.ModeChat || thread.AgentID == "" {
		t.Fatalf("thread = title %q mode %q agent %q", thread.Title, thread.Mode, thread.AgentID)
	}
	agent, found, err := f.app.store.GetStandingAgent(thread.ID)
	if err != nil || !found || agent.ID != thread.AgentID || agent.CreatorAgentID != "" {
		t.Fatalf("record = %+v %v %v", agent, found, err)
	}
	if want := filepath.Join(f.app.configDir, "agents", agent.ID); agent.Home != want {
		t.Fatalf("home = %s, want %s", agent.Home, want)
	}
	if _, err := os.Stat(filepath.Join(agent.Home, "README.md")); err != nil {
		t.Fatalf("home README: %v", err)
	}
	opening := agenthome.OpeningMessage("Ops", agent.Home)
	awaitAppMessage(t, sent, func(text string) bool { return text == opening })

	before, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	var taken *store.AssistantNameTakenError
	if _, err := f.app.CreateAssistant(t.Context(), CreateAssistantOptions{ProjectIDs: []string{f.caller.ProjectID}, Name: "OPS"}); !errors.As(err, &taken) || taken.ThreadID != thread.ID {
		t.Fatalf("a clashing name = %v, want the holder named", err)
	}
	after, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a refused creation left a thread: %d rows, had %d", len(after), len(before))
	}
}

// thread_new_assistant: only an assistant's current thread may create one;
// the new assistant records its creator, its first message is the created
// opening block as the creator's request, its questions route to the
// creator, and its sessions say who it reports to.
func TestNewAssistantToolCreatesAnAssistantThatReportsToItsCreator(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaude(t, "I am Scout.")
	call := threadtools.NewAssistantCall{Name: "Scout", Role: "Watch the build and say when it breaks."}
	if _, err := f.adapter().NewAssistant(t.Context(), f.callerIdentity(), call); err == nil || !strings.Contains(err.Error(), "Only an assistant's current thread") {
		t.Fatalf("an ordinary caller = %v, want the refusal", err)
	}

	creator := f.assistantCaller(t)
	// Settings no default gives, so the created thread shows it took them.
	if err := f.app.store.UpdateModel(f.caller.ID, "claude-sonnet-4-6"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.app.store.UpdateReasoningEffort(f.caller.ID, "low"); err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.UpdateRuntimeMode(f.caller.ID, string(provider.RuntimeAutoAcceptEdits)); err != nil {
		t.Fatal(err)
	}
	f.caller = mustThread(t, f.app, f.caller.ID)
	// A computer forwarding the call is refused here too, not only by the
	// tool server in front of it.
	if _, err := f.adapter().NewAssistant(threadtools.WithForwarded(t.Context()), f.callerIdentity(), call); err == nil || !strings.Contains(err.Error(), "never forwarded") {
		t.Fatalf("a forwarded call = %v, want the refusal", err)
	}
	ack, err := f.adapter().NewAssistant(t.Context(), f.callerIdentity(), call)
	if err != nil {
		t.Fatalf("NewAssistant: %v", err)
	}
	created, found, err := f.app.store.GetStandingAgent(ack.ThreadID)
	if err != nil || !found || created.ID != ack.AssistantID || created.CreatorAgentID != creator.ID || created.Home != ack.Home || ack.Name != "Scout" {
		t.Fatalf("created = %+v %v %v, ack %+v", created, found, err, ack)
	}
	if !ack.Notify {
		t.Error("the creator is not owed the answer")
	}
	first := awaitFirstUserText(t, f.app, ack.ThreadID)
	opening := agenthome.CreatedOpeningMessage("Scout", created.Home, f.caller.Title, f.caller.ID, call.Role)
	if !strings.HasPrefix(first, opening+"\n\n---\n") {
		t.Fatalf("first message:\n%s\nwant it to open with:\n%s", first, opening)
	}
	if !strings.Contains(first, "It spawned this thread, so your questions and approval requests go to it, not to the user.") {
		t.Fatalf("the footer does not route the new assistant's prompts to its creator:\n%s", first)
	}
	thread, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	// It runs with its creator's settings, in its own home, and is no
	// helper of the creator.
	if thread.Provider != f.caller.Provider || thread.Model != f.caller.Model || thread.ReasoningEffort != f.caller.ReasoningEffort ||
		thread.RuntimeMode != f.caller.RuntimeMode || thread.WorkspacePath != created.Home || thread.HelperOfAgentID != "" {
		t.Fatalf("created thread = provider %q model %q effort %q runtime %q workspace %q helperOf %q, creator %q %q %q %q",
			thread.Provider, thread.Model, thread.ReasoningEffort, thread.RuntimeMode, thread.WorkspacePath, thread.HelperOfAgentID,
			f.caller.Provider, f.caller.Model, f.caller.ReasoningEffort, f.caller.RuntimeMode)
	}
	agent, err := f.app.readStandingAgentSession(thread)
	if err != nil || !strings.Contains(agent.Block, `You report to the standing agent "`+f.caller.Title+`" (thread `+f.caller.ID+`), which created you.`) {
		t.Fatalf("session block = %q, %v", agent.Block, err)
	}
	// An assistant it created is its own row, never the creator's child.
	if caller, err := f.app.store.GetThread(f.caller.ID); err != nil || archivedTotal(caller.ArchivedChildCounts) != 0 {
		t.Fatalf("creator row = %+v %v", caller, err)
	}
	if listed, err := f.app.ListArchivedSpawnedThreads(f.caller.ID); err != nil || len(listed) != 0 {
		t.Fatalf("archived children = %+v %v", listed, err)
	}

	if _, err := f.adapter().NewAssistant(t.Context(), f.callerIdentity(), threadtools.NewAssistantCall{Name: "scout", Role: "again"}); err == nil || !strings.Contains(err.Error(), `"Scout"`) {
		t.Fatalf("a clashing name = %v, want the holder named", err)
	}
}

// An assistant's name addresses its current thread, ignoring case, and a
// short name is not read as an id prefix.
func TestResolveThreadRefMatchesAnAssistantName(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "ops-thread")
	testAssistant(t, f.app, thread.ID)
	if err := f.app.store.UpdateTitle(thread.ID, "Ops"); err != nil {
		t.Fatal(err)
	}
	res, err := f.adapter.ResolveThreadRef(t.Context(), "ops")
	if err != nil || len(res.Matches) != 1 || res.Matches[0].ThreadID != thread.ID || res.Matches[0].Assistant != "Ops" {
		t.Fatalf("resolve ops = %+v %v", res, err)
	}
	f.thread(t, "opsxyz")
	if res, err := f.adapter.ResolveThreadRef(t.Context(), "ops"); err != nil || len(res.Matches) != 1 {
		t.Fatalf("a short name matched an id prefix: %+v %v", res, err)
	}
}

// A send addressed to an assistant's past thread goes to its current one.
func TestSendToAPastThreadGoesToTheCurrentThread(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaude(t, "noted")
	target, _ := f.assistantTarget(t, "Reviewer of the parser.")
	successor, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	placeInAssistantHome(t, f.app, successor.ID, target.ID)
	if _, err := f.app.store.HandOffAssistant(target.ID, successor.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: target.ID, Message: "look"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ack.ThreadID != successor.ID {
		t.Fatalf("send went to %s, want the current thread %s", ack.ThreadID, successor.ID)
	}
	if row := f.request(t, ack.Token); row.TargetThreadID != successor.ID {
		t.Fatalf("request target = %s", row.TargetThreadID)
	}
}

// New Thread, Same Assistant: the record, the loops and the workers move
// to the successor, the old thread is archived as a past thread, and the
// successor opens with the block naming it.
func TestStartNextAssistantThreadCarriesItsWork(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	sent := f.mockClaudeRecording(t, "on it")
	agent := f.assistantCaller(t)
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "go", WaitSeconds: 20})
	if err != nil {
		t.Fatal(err)
	}
	loop := registerTestWake(t, f.app, store.AgentWake{ThreadID: f.caller.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "check", IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})

	successor, err := f.app.StartNextAssistantThread(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatalf("StartNextAssistantThread: %v", err)
	}
	if successor.Title != f.caller.Title || successor.AgentID != agent.ID || successor.Provider != f.caller.Provider || successor.Model != f.caller.Model {
		t.Fatalf("successor = %+v", successor)
	}
	old, err := f.app.store.GetThread(f.caller.ID)
	if err != nil || !old.Archived || old.PastAgentID != agent.ID {
		t.Fatalf("old row = %+v %v", old, err)
	}
	if row, _, _ := f.app.store.GetAgentWake(loop.ID); row.ThreadID != successor.ID {
		t.Fatalf("loop on %s", row.ThreadID)
	}
	if worker, err := f.app.store.GetThread(ack.ThreadID); err != nil || worker.SpawnedByThreadID != successor.ID || !worker.SpawnedByAgent {
		t.Fatalf("worker = %+v %v", worker, err)
	}
	first := awaitAppMessage(t, sent, func(text string) bool {
		return strings.HasPrefix(text, `You are "`+f.caller.Title+`", a standing agent. The user replaced your previous thread `+f.caller.ID+` with this one`)
	})
	if !strings.Contains(first, "1 loop or watch") {
		t.Fatalf("opening block:\n%s", first)
	}
	if _, err := f.app.StartNextAssistantThread(t.Context(), f.caller.ID); !errors.Is(err, errNotAssistantThread) {
		t.Fatalf("a second handoff of the past thread = %v", err)
	}
}

// A Claude assistant whose transcript the provider cleaned up is moved to a
// new thread by the start that finds it, and the new thread's opening says
// why and names the previous thread.
func TestASessionTheProviderCleanedUpMovesTheAssistant(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	sent := f.mockClaudeRecording(t, "picked up")
	thread, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	testAssistant(t, f.app, thread.ID)
	if _, err := f.app.store.UpdateSessionRef(thread.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	err = f.app.StartSession(thread.ID)
	if !errors.Is(err, errAssistantSessionGone) {
		t.Fatalf("StartSession = %v, want errAssistantSessionGone", err)
	}
	if successor := awaitCurrentThread(t, f.app, thread.ID); successor == thread.ID {
		t.Fatal("the assistant stayed on the thread whose session is gone")
	}
	first := awaitAppMessage(t, sent, func(text string) bool { return strings.HasPrefix(text, `You are "`) })
	if !strings.Contains(first, "The provider cleaned up the session of your previous thread "+thread.ID+", so it could not resume") ||
		!strings.Contains(first, "Agent Overflow's history of the previous thread is intact.") {
		t.Fatalf("opening block:\n%s", first)
	}

	// An ordinary thread's missing transcript is the provider's own error,
	// not a handoff.
	plain, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.requireAssistantSession(plain, provider.SessionOptions{Resume: uuid.NewString()}); err != nil {
		t.Fatalf("an ordinary thread was checked: %v", err)
	}
}

// An assistant's row is announced, at creation and at a handoff, while the
// operation still holds a work admission, so no host handoff starts between
// the row and the record behind it.
func TestAssistantRowsAreAnnouncedUnderWorkAdmission(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaudeRecording(t, "ready")
	var mu sync.Mutex
	announced := map[string]string{}
	f.app.testEmitHook = func(name string, data any) {
		event, ok := data.(triage.ThreadUpdateEvent)
		if name != "thread:updated" || !ok || event.Action != triage.ThreadActionListed || event.Thread == nil || event.Thread.AgentID == "" {
			return
		}
		reason, err := f.app.workAdmission.quiesce(func() (string, error) { return "", nil })
		if err == nil && reason == "" {
			f.app.workAdmission.reopen()
		}
		mu.Lock()
		announced[event.Thread.ID] = reason
		mu.Unlock()
	}
	thread, err := f.app.CreateAssistant(t.Context(), CreateAssistantOptions{ProjectIDs: []string{f.caller.ProjectID}, Name: "Ops"})
	if err != nil {
		t.Fatalf("CreateAssistant: %v", err)
	}
	successor, err := f.app.StartNextAssistantThread(t.Context(), thread.ID)
	if err != nil {
		t.Fatalf("StartNextAssistantThread: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{thread.ID, successor.ID} {
		if reason, found := announced[id]; !found || reason == "" {
			t.Errorf("row %s announced=%v outside a work admission", id, found)
		}
	}
}

// A name in use is refused with the code the inline editors branch on, naming
// the assistant that holds it, from creation and from a rename alike; the
// refused rename leaves the title as it was.
func TestATakenAssistantNameIsRefusedWithItsCode(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaude(t, "Ready.")
	ops, err := f.app.CreateAssistant(t.Context(), CreateAssistantOptions{ProjectIDs: []string{f.caller.ProjectID}, Name: "Ops"})
	if err != nil {
		t.Fatal(err)
	}
	scout, err := f.app.CreateAssistant(t.Context(), CreateAssistantOptions{ProjectIDs: []string{f.caller.ProjectID}, Name: "Scout"})
	if err != nil {
		t.Fatal(err)
	}
	wantMessage := `The assistant "Ops" already has that name; names are unique on this computer.`
	_, err = f.app.CreateAssistant(t.Context(), CreateAssistantOptions{ProjectIDs: []string{f.caller.ProjectID}, Name: "ops"})
	if code, message, ok := errorsx.PublicDetails(err); !ok || code != transport.ErrCodeAssistantNameTaken || message != wantMessage {
		t.Fatalf("creation refusal = %q %q %v (%v)", code, message, ok, err)
	}
	err = f.app.RenameThread(scout.ID, " OPS ")
	if code, message, ok := errorsx.PublicDetails(err); !ok || code != transport.ErrCodeAssistantNameTaken || message != wantMessage {
		t.Fatalf("rename refusal = %q %q %v (%v)", code, message, ok, err)
	}
	row, err := f.app.store.GetThread(scout.ID)
	if err != nil || row.Title != "Scout" {
		t.Fatalf("refused rename left title %q (%v)", row.Title, err)
	}
	if ops.ID == scout.ID {
		t.Fatal("two assistants share a thread")
	}
}

// Codex has no transcript to look for: its thread/resume refusal with no
// rollout is the signal, and the assistant moves the same way. An ordinary
// Codex thread's refusal stays the provider's own error.
func TestACodexSessionTheProviderCleanedUpMovesTheAssistant(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	if _, err := f.app.settings.Update(map[string]any{"codexBinaryPath": writeCodexNoRolloutBinary(t)}); err != nil {
		t.Fatal(err)
	}
	thread, err := createTestThread(t, f.app, string(provider.Codex), t.TempDir(), "gpt-5", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	testAssistant(t, f.app, thread.ID)
	if _, err := f.app.store.UpdateSessionRef(thread.ID, "gone-rollout"); err != nil {
		t.Fatal(err)
	}

	if err := f.app.StartSession(thread.ID); !errors.Is(err, errAssistantSessionGone) {
		t.Fatalf("StartSession = %v, want errAssistantSessionGone", err)
	}
	if successor := awaitCurrentThread(t, f.app, thread.ID); successor == thread.ID {
		t.Fatal("the assistant stayed on the thread whose session is gone")
	}

	plain, err := createTestThread(t, f.app, string(provider.Codex), t.TempDir(), "gpt-5", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.store.UpdateSessionRef(plain.ID, "gone-rollout"); err != nil {
		t.Fatal(err)
	}
	if err := f.app.StartSession(plain.ID); err == nil || errors.Is(err, errAssistantSessionGone) {
		t.Fatalf("an ordinary thread's refused resume = %v, want the provider's own error", err)
	}
}

// writeCodexNoRolloutBinary is a Codex app-server whose thread/resume finds
// no rollout, as after the provider cleaned up the thread's session file.
func writeCodexNoRolloutBinary(t *testing.T) string {
	t.Helper()
	script := `#!/bin/sh
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/resume"'; then
        printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32600,"message":"no rollout found for thread id gone-rollout"}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"fresh-rollout"}}}\n' "$id"
        continue
    fi
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
done
`
	path := filepath.Join(t.TempDir(), "codex-no-rollout.sh")
	mockexec.Write(t, path, script)
	return path
}
