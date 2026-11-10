package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/shellrun"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"

	"github.com/google/uuid"
)

func newShellCommandApp(t *testing.T) (*App, *emitRecorder) {
	t.Helper()
	app := newMixedTurnApp(t)
	rec := &emitRecorder{}
	app.emitEventFn = rec.capture
	if err := app.initShellCommands(t.TempDir()); err != nil {
		t.Fatalf("initShellCommands: %v", err)
	}
	t.Cleanup(app.stopShellCommands)
	t.Setenv("SHELL", "/bin/sh")
	return app, rec
}

func shellFrames(rec *emitRecorder, runID string) []ShellCommandEvent {
	var frames []ShellCommandEvent
	for _, call := range rec.snapshot() {
		if call.Channel != eventchan.ShellCommand.String() {
			continue
		}
		if frame, ok := call.Data.(ShellCommandEvent); ok && frame.RunID == runID {
			frames = append(frames, frame)
		}
	}
	return frames
}

func waitShellFinished(t *testing.T, rec *emitRecorder, runID string) ShellCommandEvent {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, frame := range shellFrames(rec, runID) {
			if frame.Phase == shellrun.PhaseFinished {
				return frame
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for run %s to finish; frames: %+v", runID, shellFrames(rec, runID))
	return ShellCommandEvent{}
}

func userItems(t *testing.T, app *App, threadID string) []store.Item {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var out []store.Item
	for _, item := range items {
		if item.Role == "user" {
			out = append(out, item)
		}
	}
	return out
}

// The whole contract on an idle thread: the command runs in the workspace,
// its output reaches the provider as Claude's own `!` record, and the row
// keeps what the person typed as its text beside the attachment metadata.
func TestRunThreadShellCommandSendsTheOutputAsAUserMessage(t *testing.T) {
	app, rec := newShellCommandApp(t)
	thread, capturePath := newMixedTurnThread(t, app, "thread-shell-send")
	runID := uuid.NewString()

	started, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
		RunID: runID, Command: "  printf 'in %s\\n' \"$(basename \"$PWD\")\"; echo '<tag>'  ",
	})
	if err != nil {
		t.Fatalf("RunThreadShellCommand: %v", err)
	}
	if started.State != shellrun.StateRunning || started.RunID != runID {
		t.Fatalf("start state: %+v", started)
	}
	if final := waitShellFinished(t, rec, runID); final.State != shellrun.StateSent || final.ExitCode != 0 {
		t.Fatalf("terminal frame: %+v", final)
	}

	texts := waitForCapturedUserMessages(t, capturePath, 1)
	workspace := thread.WorkspacePath[strings.LastIndex(thread.WorkspacePath, "/")+1:]
	command := `printf 'in %s\n' "$(basename "$PWD")"; echo '<tag>'`
	want := "<bash-input>" + command + "</bash-input>\n<bash-stdout>in " + workspace + "\n&lt;tag&gt;</bash-stdout><bash-stderr></bash-stderr>"
	if texts[0] != want {
		t.Fatalf("wire text =\n%q\nwant\n%q", texts[0], want)
	}

	rows := userItems(t, app, thread.ID)
	if len(rows) != 1 || rows[0].Summary != "!"+command {
		t.Fatalf("user rows: %+v", rows)
	}
	meta, err := usermessageMetaFromItem(rows[0])
	if err != nil || len(meta.Attachments) != 1 || meta.Attachments[0].Shell == nil {
		t.Fatalf("row meta: %+v %v", meta, err)
	}
	shell := meta.Attachments[0].Shell
	if shell.Command != command || shell.ExitCode != 0 || shell.Preview != "in "+workspace+"\n<tag>\n" || shell.More {
		t.Fatalf("shell meta: %+v", shell)
	}
	if meta.SendID != runID {
		t.Fatalf("row send id = %q, want the run id", meta.SendID)
	}

	page, err := app.ReadShellAttachmentOutput(thread.ID, meta.Attachments[0].ID, 0, 0)
	if err != nil || page.Data != "in "+workspace+"\n<tag>\n" || page.Command != command || page.Next != page.Size {
		t.Fatalf("output page: %+v %v", page, err)
	}

	// A request repeated after the command settled neither runs nor sends.
	again, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{RunID: runID, Command: command})
	if err != nil || again.State != shellrun.StateSent {
		t.Fatalf("repeated request: %+v %v", again, err)
	}
	time.Sleep(150 * time.Millisecond)
	if texts := capturedUserMessageTexts(t, capturePath); len(texts) != 1 {
		t.Fatalf("repeated request sent again: %q", texts)
	}
	if runs, _ := app.GetThreadShellCommands(thread.ID); len(runs) != 0 {
		t.Fatalf("repeated request started a run: %+v", runs)
	}
}

func TestRunThreadShellCommandSendsAFailureWithItsExitCode(t *testing.T) {
	app, rec := newShellCommandApp(t)
	thread, capturePath := newMixedTurnThread(t, app, "thread-shell-fail")
	runID := uuid.NewString()
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
		RunID: runID, Command: "echo boom 1>&2; exit 7",
	}); err != nil {
		t.Fatal(err)
	}
	if final := waitShellFinished(t, rec, runID); final.State != shellrun.StateSent || final.ExitCode != 7 {
		t.Fatalf("terminal frame: %+v", final)
	}
	texts := waitForCapturedUserMessages(t, capturePath, 1)
	want := "<bash-input>echo boom 1>&2; exit 7</bash-input>\n<bash-stdout></bash-stdout><bash-stderr>boom\nExit code 7</bash-stderr>"
	if texts[0] != want {
		t.Fatalf("wire text =\n%q\nwant\n%q", texts[0], want)
	}
	meta, err := usermessageMetaFromItem(userItems(t, app, thread.ID)[0])
	if err != nil || meta.Attachments[0].Shell.ExitCode != 7 {
		t.Fatalf("row meta: %+v %v", meta, err)
	}
}

// A command that settles while a turn is running joins the ordinary queue
// with its attachment, like any composer message.
func TestRunThreadShellCommandQueuesWhileATurnIsActive(t *testing.T) {
	app, rec := newShellCommandApp(t)
	thread, capturePath := newMixedTurnThread(t, app, "thread-shell-queue")
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventTurnStart: %v", err)
	}
	runID := uuid.NewString()
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
		RunID: runID, Command: "echo queued",
	}); err != nil {
		t.Fatal(err)
	}
	if final := waitShellFinished(t, rec, runID); final.State != shellrun.StateSent {
		t.Fatalf("terminal frame: %+v", final)
	}
	texts := waitForCapturedUserMessages(t, capturePath, 1)
	if want := "<bash-input>echo queued</bash-input>\n<bash-stdout>queued</bash-stdout><bash-stderr></bash-stderr>"; texts[0] != want {
		t.Fatalf("queued wire text =\n%q\nwant\n%q", texts[0], want)
	}
}

func TestStopThreadShellCommandSendsNothing(t *testing.T) {
	app, rec := newShellCommandApp(t)
	thread, capturePath := newMixedTurnThread(t, app, "thread-shell-stop")
	runID := uuid.NewString()
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
		RunID: runID, Command: "echo started; sleep 60",
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs, err := app.GetThreadShellCommands(thread.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && strings.Contains(runs[0].Output, "started") && runs[0].OutputSeq > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("command never reported output: %+v", runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
		RunID: uuid.NewString(), Command: "echo second",
	}); !errors.Is(err, shellrun.ErrBusy) {
		t.Fatalf("second command while one runs: %v", err)
	}
	// The thread's live state names the run, and no output, for a client
	// that mounts while it is running.
	if live, err := app.GetThreadLiveState(thread.ID); err != nil || live.ShellCommandRunID != runID {
		t.Fatalf("live state while running: %q %v", live.ShellCommandRunID, err)
	}
	if err := app.StopThreadShellCommand(thread.ID, runID); err != nil {
		t.Fatal(err)
	}
	if final := waitShellFinished(t, rec, runID); final.State != shellrun.StateStopped {
		t.Fatalf("terminal frame: %+v", final)
	}
	if live, err := app.GetThreadLiveState(thread.ID); err != nil || live.ShellCommandRunID != "" {
		t.Fatalf("live state after the stop: %q %v", live.ShellCommandRunID, err)
	}
	time.Sleep(150 * time.Millisecond)
	if texts := capturedUserMessageTexts(t, capturePath); len(texts) != 0 {
		t.Fatalf("a stopped command was sent: %q", texts)
	}
	if rows := userItems(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("a stopped command left a row: %+v", rows)
	}
	if attachments, err := app.attachments.List(thread.ID); err != nil || len(attachments) != 0 {
		t.Fatalf("a stopped command left an attachment: %+v %v", attachments, err)
	}
	if err := app.StopThreadShellCommand(thread.ID, runID); !errors.Is(err, shellrun.ErrNotRunning) {
		t.Fatalf("stop of a settled run: %v", err)
	}
}

// A result the send path refuses leaves no attachment behind and says why on
// the terminal frame.
func TestRunThreadShellCommandRemovesTheOutputWhenTheSendFails(t *testing.T) {
	app, rec := newShellCommandApp(t)
	thread, _ := newMixedTurnThread(t, app, "thread-shell-send-fails")
	app.sendMessageFn = func(string, string, []string) error { return errors.New("provider refused") }
	runID := uuid.NewString()
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
		RunID: runID, Command: "echo lost",
	}); err != nil {
		t.Fatal(err)
	}
	final := waitShellFinished(t, rec, runID)
	if final.State != shellrun.StateFailed || !strings.Contains(final.Error, "was not sent") ||
		!strings.Contains(final.Error, "provider refused") {
		t.Fatalf("terminal frame: %+v", final)
	}
	if attachments, err := app.attachments.List(thread.ID); err != nil || len(attachments) != 0 {
		t.Fatalf("unsent output kept: %+v %v", attachments, err)
	}
}

// A send can fail after its row is written: the session would not start. The
// row shows the command's output, so the output stays.
func TestRunThreadShellCommandKeepsTheOutputOfAMessageThatWasRecorded(t *testing.T) {
	app, rec := newShellCommandApp(t)
	thread := newClaudeThreadForProviderCommandTest(t, app, "thread-shell-row-kept")
	app.startSessionFn = func(string) error { return errors.New("no provider today") }
	runID := uuid.NewString()
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
		RunID: runID, Command: "echo kept",
	}); err != nil {
		t.Fatal(err)
	}
	final := waitShellFinished(t, rec, runID)
	if final.State != shellrun.StateFailed || !strings.Contains(final.Error, "no provider today") {
		t.Fatalf("terminal frame: %+v", final)
	}
	rows := userItems(t, app, thread.ID)
	if len(rows) != 1 || rows[0].Summary != "!echo kept" {
		t.Fatalf("rows: %+v", rows)
	}
	attachments, err := app.attachments.List(thread.ID)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("the row's output: %+v %v", attachments, err)
	}
	page, err := app.ReadShellAttachmentOutput(thread.ID, attachments[0].ID, 0, 0)
	if err != nil || page.Data != "kept\n" {
		t.Fatalf("the row's output reads: %+v %v", page, err)
	}
}

// An empty draft thread is deleted when its composer is cleared, and running
// a command clears the composer. The thread stays while the command runs.
func TestAnEmptyDraftThreadIsKeptWhileItsCommandRuns(t *testing.T) {
	app, rec := newShellCommandApp(t)
	thread := newClaudeThreadForProviderCommandTest(t, app, "thread-shell-empty-draft")
	app.startSessionFn = func(string) error { return errors.New("no provider today") }
	runID := uuid.NewString()
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
		RunID: runID, Command: "sleep 60",
	}); err != nil {
		t.Fatal(err)
	}
	if deleted, err := app.DeleteEmptyDraftThread(thread.ID); err != nil || deleted {
		t.Fatalf("delete while the command runs: %v %v", deleted, err)
	}
	if err := app.StopThreadShellCommand(thread.ID, runID); err != nil {
		t.Fatal(err)
	}
	waitShellFinished(t, rec, runID)
	if deleted, err := app.DeleteEmptyDraftThread(thread.ID); err != nil || !deleted {
		t.Fatalf("delete after the command stopped: %v %v", deleted, err)
	}
}

// A start repeated after its run settled is answered from how it settled.
func TestARepeatedStartDoesNotRunASettledCommandAgain(t *testing.T) {
	app, rec := newShellCommandApp(t)
	thread, _ := newMixedTurnThread(t, app, "thread-shell-repeat")
	marker := filepath.Join(t.TempDir(), "ran")
	request := ShellCommandRequest{RunID: uuid.NewString(), Command: "echo once >> " + marker}
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, request); err != nil {
		t.Fatal(err)
	}
	if final := waitShellFinished(t, rec, request.RunID); final.State != shellrun.StateSent {
		t.Fatalf("terminal frame: %+v", final)
	}
	again, err := app.RunThreadShellCommand(context.Background(), thread.ID, request)
	if err != nil || again.State != shellrun.StateSent {
		t.Fatalf("repeated start: %+v %v", again, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "once\n" {
		t.Fatalf("the command ran %q %v", data, err)
	}
}

// An update waits for a running command and says what it is waiting for.
func TestAnUpdateNamesARunningShellCommand(t *testing.T) {
	app, rec := newShellCommandApp(t)
	thread, _ := newMixedTurnThread(t, app, "thread-shell-update")
	runID := uuid.NewString()
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
		RunID: runID, Command: "sleep 60",
	}); err != nil {
		t.Fatal(err)
	}
	if reason, err := app.updateWorkReason(); err != nil || !strings.Contains(reason, "shell commands") {
		t.Fatalf("reason while a command runs: %q %v", reason, err)
	}
	if err := app.StopThreadShellCommand(thread.ID, runID); err != nil {
		t.Fatal(err)
	}
	waitShellFinished(t, rec, runID)
	if reason, err := app.updateWorkReason(); err != nil || strings.Contains(reason, "shell commands") {
		t.Fatalf("reason after it stopped: %q %v", reason, err)
	}
}

// The first prompt names a temporary worktree branch. A command says what
// was run, not what the work is, so it leaves the name to the prompt.
func TestACommandDoesNotNameTheWorktreeBranch(t *testing.T) {
	app, rec := newShellCommandApp(t)
	var named []string
	app.generateBranchNameFn = func(_ store.Thread, message string) (string, error) {
		named = append(named, message)
		return "", nil
	}
	temporaryWorktreeThread := func(id string) store.Thread {
		thread := testThread(id)
		thread.Provider = string(provider.Claude)
		thread.WorkspacePath = t.TempDir()
		thread.WorktreePath = thread.WorkspacePath
		thread.Branch = app.worktreeBranchPrefix() + "1a2b3c4d"
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		installCapturingClaudeSession(t, app, thread, filepath.Join(t.TempDir(), "stdin.ndjson"))
		return thread
	}
	commanded := temporaryWorktreeThread("thread-shell-branch")
	runID := uuid.NewString()
	if _, err := app.RunThreadShellCommand(context.Background(), commanded.ID, ShellCommandRequest{
		RunID: runID, Command: "npm install",
	}); err != nil {
		t.Fatal(err)
	}
	if final := waitShellFinished(t, rec, runID); final.State != shellrun.StateSent {
		t.Fatalf("terminal frame: %+v", final)
	}
	if len(named) != 0 {
		t.Fatalf("a command named the branch from %q", named)
	}
	// The same thread shape names its branch from a prompt.
	prompted := temporaryWorktreeThread("thread-prompt-branch")
	if err := app.SendMessage(prompted.ID, "add the settings page", nil); err != nil {
		t.Fatal(err)
	}
	if len(named) != 1 || named[0] != "add the settings page" {
		t.Fatalf("a prompt did not name the branch: %q", named)
	}
}

func TestRunThreadShellCommandRefusesBadRequests(t *testing.T) {
	app, _ := newShellCommandApp(t)
	thread, _ := newMixedTurnThread(t, app, "thread-shell-refuse")
	for name, request := range map[string]ShellCommandRequest{
		"empty command": {RunID: uuid.NewString(), Command: "   "},
		"no run id":     {Command: "echo hi"},
	} {
		if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, request); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := app.RunThreadShellCommand(context.Background(), "missing-thread", ShellCommandRequest{
		RunID: uuid.NewString(), Command: "echo hi",
	}); err == nil {
		t.Error("a missing thread was accepted")
	}
	gone := testThread("thread-shell-no-dir")
	gone.Provider = string(provider.Claude)
	gone.WorkspacePath = t.TempDir() + "/removed"
	if err := app.store.CreateThread(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := app.RunThreadShellCommand(context.Background(), gone.ID, ShellCommandRequest{
		RunID: uuid.NewString(), Command: "echo hi",
	}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("a missing workspace: %v", err)
	}
}

func TestUserShellPromptBlockUsesTheProvidersOwnRecord(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/output.txt"
	if err := os.WriteFile(path, []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := store.Attachment{Kind: store.AttachmentKindFile, Shell: store.AttachmentShell{Command: "echo hi", DurationMs: 20}}
	claudeBlock, err := userShellPromptBlock(string(provider.Claude), record, path)
	if err != nil || claudeBlock != "<bash-input>echo hi</bash-input>\n<bash-stdout>hi</bash-stdout><bash-stderr></bash-stderr>" {
		t.Fatalf("claude block: %q %v", claudeBlock, err)
	}
	codexBlock, err := userShellPromptBlock(string(provider.Codex), record, path)
	want := "<user_shell_command>\n<command>\necho hi\n</command>\n<result>\nExit code: 0\nDuration: 0.0200 seconds\nOutput:\nhi\n\n</result>\n</user_shell_command>"
	if err != nil || codexBlock != want {
		t.Fatalf("codex block: %q %v", codexBlock, err)
	}
}

// The typed `!command` is a transcript fact. On the wire the command block
// stands where it was typed; a block whose invocation is not in the text, and
// every ordinary file line, follows the text.
func TestPromptTextPutsACommandBlockWhereItWasTyped(t *testing.T) {
	t.Parallel()
	separator := usermessage.JoinSeparator
	attachments := turnAttachments{
		fileLines:   []string{"[file a]", "<make>", "<make again>", "<ls>"},
		invocations: []string{"", "!make test", "!make test", "!ls"},
	}
	for name, tc := range map[string]struct{ text, want string }{
		"the command alone": {"!ls", "<ls>\n\n[file a]\n<make>\n<make again>"},
		"joined around other text": {
			"!make test" + separator + "fix those" + separator + " !ls \n",
			"<make>" + separator + "fix those" + separator + "<ls>\n\n[file a]\n<make again>",
		},
		"the same command twice": {
			"!make test" + separator + "!make test",
			"<make>" + separator + "<make again>\n\n[file a]\n<ls>",
		},
		"edited text keeps its words": {
			"!ls and explain", "!ls and explain\n\n[file a]\n<make>\n<make again>\n<ls>",
		},
		"no text": {"", "[file a]\n<make>\n<make again>\n<ls>"},
	} {
		if got := attachments.promptText([]string{tc.text}, ""); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
	// A command can hold the join rule itself, as a heredoc of YAML does.
	heredoc := "cat <<EOF\na: 1" + separator + "b: 2\nEOF"
	spanning := turnAttachments{fileLines: []string{"<heredoc>"}, invocations: []string{"!" + heredoc}}
	for name, tc := range map[string]struct{ text, want string }{
		"alone":            {"!" + heredoc, "<heredoc>"},
		"joined with text": {"first" + separator + "!" + heredoc + separator + "last", "first" + separator + "<heredoc>" + separator + "last"},
		"only its start":   {"!cat <<EOF\na: 1", "!cat <<EOF\na: 1\n\n<heredoc>"},
	} {
		if got := spanning.promptText([]string{tc.text}, ""); got != tc.want {
			t.Errorf("spanning, %s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
	if !spanning.isInvocation("!" + heredoc) {
		t.Error("a command holding the join rule is not recognized as the invocation")
	}
	// Two messages sent together never combine into one invocation.
	if got := spanning.promptText([]string{"!cat <<EOF\na: 1", "b: 2\nEOF"}, "\n\n"); strings.HasPrefix(got, "<heredoc>") {
		t.Errorf("two messages were read as one command: %q", got)
	}
	// A restored command is sent as a message again; a `/word` in it is a
	// path, so it is the invocation and not prose with a composer command.
	for content, want := range map[string]bool{
		"!ls": true, " !make test\n": true, "!ls and explain": false, "": false, "ls": false,
	} {
		if got := attachments.isInvocation(content); got != want {
			t.Errorf("isInvocation(%q) = %v, want %v", content, got, want)
		}
	}
	parts := []string{"!ls", "then this"}
	if got := attachments.promptText(parts, "\n\n"); !strings.HasPrefix(got, "<ls>\n\nthen this\n\n") || parts[0] != "!ls" {
		t.Fatalf("separate parts: %q (caller's slice %q)", got, parts)
	}
}

func TestDeletingAThreadStopsItsShellCommand(t *testing.T) {
	app, rec := newShellCommandApp(t)
	thread, _ := newMixedTurnThread(t, app, "thread-shell-delete")
	runID := uuid.NewString()
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
		RunID: runID, Command: "sleep 60",
	}); err != nil {
		t.Fatal(err)
	}
	app.threadDeletePorts().CancelShellCommand(thread.ID)
	if frames := shellFrames(rec, runID); len(frames) == 0 {
		t.Fatal("no frames")
	}
	if final := waitShellFinished(t, rec, runID); final.State != shellrun.StateStopped {
		t.Fatalf("terminal frame: %+v", final)
	}
}

// A command's message taken back to the composer is sent again as an ordinary
// message: its text is the typed line and it carries the output file. The
// provider reads the same block as the first time, the typed line never
// reaches it, and a `/word` in the command is not a composer command.
func TestResendingACommandMessageSendsTheSameBlock(t *testing.T) {
	app, _ := newShellCommandApp(t)
	thread, capturePath := newMixedTurnThread(t, app, "thread-shell-resend")
	capture := filepath.Join(t.TempDir(), "capture.log")
	if err := os.WriteFile(capture, []byte("etc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := app.attachments.SaveShellOutput(thread.ID,
		store.AttachmentShell{Command: "ls /workflow", DurationMs: 3}, capture, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The bound composer entry point, which opts into command expansion.
	if err := app.SendMessage(thread.ID, "!ls /workflow", []string{record.ID}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	texts := waitForCapturedUserMessages(t, capturePath, 1)
	want := "<bash-input>ls /workflow</bash-input>\n<bash-stdout>etc</bash-stdout><bash-stderr></bash-stderr>"
	if texts[0] != want {
		t.Fatalf("wire text =\n%q\nwant\n%q", texts[0], want)
	}
	rows := userItems(t, app, thread.ID)
	if len(rows) != 1 || rows[0].Summary != "!ls /workflow" {
		t.Fatalf("user rows: %+v", rows)
	}
	if meta, err := usermessageMetaFromItem(rows[0]); err != nil || meta.Command != "" {
		t.Fatalf("a path was recorded as a composer command: %+v %v", meta, err)
	}
}
