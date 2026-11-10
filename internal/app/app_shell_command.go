package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/appimage"
	attachmentstore "agent-overflow/internal/attachment"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/shellrun"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/usermessage"
)

// Composer shell commands: `!command` in a thread's composer, and the Run
// action on a shell code block. The app runs the command in the thread's
// workspace (internal/shellrun), stores its output as a `file` attachment
// that records the command (store.AttachmentShell), and sends that
// attachment as an ordinary user message, so queueing, retry, restore and
// transfer are the send path's own. The provider reads the output in the
// form its CLI records a command the person ran (userShellPromptBlock).

// ShellCommandRequest starts one composer shell command.
type ShellCommandRequest struct {
	// RunID is the client-minted identity of the run. It is also the send id
	// of the message the result becomes, so a repeated request neither runs
	// the command twice nor sends its output twice.
	RunID   string `json:"runId"`
	Command string `json:"command"`
}

// ShellCommandRun is a running command: the answer to a start and the
// snapshot a client reads at mount and after a transport gap.
type ShellCommandRun struct {
	ThreadID  string `json:"threadId"`
	RunID     string `json:"runId"`
	Command   string `json:"command"`
	State     string `json:"state"`
	Output    string `json:"output"`
	OutputSeq uint64 `json:"outputSeq"`
	// OutputTruncated reports that Output is the end of a longer stream.
	OutputTruncated bool  `json:"outputTruncated,omitempty"`
	StartedAt       int64 `json:"startedAt"`
}

// ShellCommandEvent is one frame on the `shell:command` channel. Phase is
// `started`, `output` or `finished`. Output frames carry a per-run Seq that
// starts at 1; a client that has applied a snapshot drops frames at or below
// the snapshot's OutputSeq. A finished frame's State is `sent`, `stopped` or
// `failed`, and Error is set on `failed`.
type ShellCommandEvent struct {
	Phase      string `json:"phase"`
	ThreadID   string `json:"threadId"`
	RunID      string `json:"runId"`
	Command    string `json:"command,omitempty"`
	Seq        uint64 `json:"seq,omitempty"`
	Chunk      string `json:"chunk,omitempty"`
	State      string `json:"state,omitempty"`
	Error      string `json:"error,omitempty"`
	ExitCode   int    `json:"exitCode"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
}

// ShellOutputPage is one bounded read of a shell output attachment, with the
// whole command its timeline row may hold only the start of.
type ShellOutputPage struct {
	Command string `json:"command"`
	Data    string `json:"data"`
	Offset  int64  `json:"offset"`
	Size    int64  `json:"size"`
	Next    int64  `json:"next"`
}

func (a *App) initShellCommands(dataDir string) error {
	service, err := shellrun.New(shellrun.Config{
		Dir:           filepath.Join(dataDir, "shell-runs"),
		Emit:          a.emitShellCommandEvent,
		Finish:        a.finishShellCommand,
		Context:       a.lifeCtx,
		ShutdownError: ErrShuttingDown,
	})
	if err != nil {
		return err
	}
	a.shellCommands = service
	return nil
}

func (a *App) emitShellCommandEvent(event shellrun.Event) {
	a.emitEvent(eventchan.ShellCommand, ShellCommandEvent{
		Phase: event.Phase, ThreadID: event.ThreadID, RunID: event.RunID, Command: event.Command,
		Seq: event.Seq, Chunk: event.Chunk, State: event.State, Error: event.Error,
		ExitCode: event.ExitCode, StartedAt: event.StartedAt, FinishedAt: event.FinishedAt,
	})
}

func projectShellCommandRun(state shellrun.RunState) ShellCommandRun {
	return ShellCommandRun{
		ThreadID: state.ThreadID, RunID: state.RunID, Command: state.Command, State: state.State,
		Output: state.Output, OutputSeq: state.OutputSeq, OutputTruncated: state.OutputTruncated,
		StartedAt: state.StartedAt,
	}
}

// RunThreadShellCommand runs a shell command the person typed in the thread's
// workspace and, when it exits, sends its output to the thread as a user
// message. It returns once the command is registered; output and the outcome
// arrive on `shell:command`. One command runs per thread at a time. The
// command runs with the person's own environment and no sandbox, whatever the
// thread's runtime mode: the mode governs the agent, not the person.
//
// LocalOnly: this executes an arbitrary command. RCE-equivalent.
//
//ao:scope terminal:operate
func (a *App) RunThreadShellCommand(ctx context.Context, threadID string, request ShellCommandRequest) (ShellCommandRun, error) {
	if a.shuttingDown.Load() {
		return ShellCommandRun{}, ErrShuttingDown
	}
	request.Command = strings.TrimSpace(request.Command)
	if err := a.requireScope(ctx, transport.ScopeThreadsOperate, "RunThreadShellCommand sends the command's output to the thread"); err != nil {
		return ShellCommandRun{}, err
	}
	if err := a.requireAutonomyForThread(ctx, threadID, ""); err != nil {
		return ShellCommandRun{}, err
	}
	if a.shellCommands == nil || a.attachments == nil {
		return ShellCommandRun{}, errors.New("shell commands are not available")
	}
	// Held only while the run is registered. A running command is named by
	// updateWorkReason, so an update waits for it without this lease.
	endAdmission, err := a.workAdmission.begin(ctx)
	if err != nil {
		return ShellCommandRun{}, err
	}
	defer endAdmission()
	// A thread delete drops the row under this lock and stops whatever the
	// thread is running first, so a command registered here is either
	// stopped by that delete or refused because the thread is gone.
	unlockMutation, err := a.threadApplication().LockMutable(ctx, threadID)
	if err != nil {
		return ShellCommandRun{}, err
	}
	defer unlockMutation()
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return ShellCommandRun{}, fmt.Errorf("run shell command: load thread: %w", err)
	}
	if err := a.store.CheckThreadExecutionAccess(thread); err != nil {
		return ShellCommandRun{}, err
	}
	if thread.Mode == threadmode.ModeTerminal {
		return ShellCommandRun{}, errors.New("run shell command: a terminal thread has no agent to send output to")
	}
	// A request repeated after its command settled must not run it again:
	// the run id is the send id, and the message it became is the record.
	if _, found, err := a.findRecordedSend(threadID, request.RunID); err != nil {
		return ShellCommandRun{}, fmt.Errorf("run shell command: %w", err)
	} else if found {
		return ShellCommandRun{
			ThreadID: threadID, RunID: request.RunID, Command: request.Command,
			State: shellrun.StateSent,
		}, nil
	}
	info, err := os.Stat(thread.WorkspacePath)
	if err != nil || !info.IsDir() {
		return ShellCommandRun{}, fmt.Errorf("run shell command: the thread's workspace %s is not a directory", thread.WorkspacePath)
	}
	state, err := a.shellCommands.Start(shellrun.Request{
		ThreadID: threadID, RunID: request.RunID, Command: request.Command,
		Dir: thread.WorkspacePath, Shell: userShell(), Env: userShellEnvironment(),
	})
	if err != nil {
		return ShellCommandRun{}, err
	}
	return projectShellCommandRun(state), nil
}

// StopThreadShellCommand stops a running command. Its output is discarded and
// nothing is sent.
//
//ao:scope terminal:operate
func (a *App) StopThreadShellCommand(threadID, runID string) error {
	if a.shellCommands == nil {
		return shellrun.ErrNotRunning
	}
	return a.shellCommands.StopRun(threadID, runID)
}

// GetThreadShellCommands returns the thread's running command, if any, with
// the output it has produced so far. It is the reconnect companion of the
// `shell:command` stream.
//
// LocalOnly: the payload is the output of a local command, the same data
// class as GetTerminalReplay.
//
//ao:scope terminal:operate
func (a *App) GetThreadShellCommands(threadID string) ([]ShellCommandRun, error) {
	runs := []ShellCommandRun{}
	if a.shellCommands == nil {
		return runs, nil
	}
	for _, state := range a.shellCommands.Active(threadID) {
		runs = append(runs, projectShellCommandRun(state))
	}
	return runs, nil
}

// ReadShellAttachmentOutput returns one bounded page of the output a shell
// command's timeline row shows. The output was sent to the agent as part of
// the conversation, so it reads with the conversation.
//
//ao:scope threads:read
func (a *App) ReadShellAttachmentOutput(threadID, attachmentID string, offset int64, limit int) (ShellOutputPage, error) {
	if a.attachments == nil {
		return ShellOutputPage{}, fmt.Errorf("attachment store not initialized")
	}
	record, chunk, err := a.attachments.ReadShellOutput(threadID, attachmentID, offset, limit)
	if err != nil {
		return ShellOutputPage{}, err
	}
	return ShellOutputPage{
		Command: record.Shell.Command, Data: chunk.Data, Offset: chunk.Offset, Size: chunk.Size, Next: chunk.Next,
	}, nil
}

func (a *App) cancelThreadShellCommand(threadID string) {
	if a.shellCommands != nil {
		a.shellCommands.CancelThread(threadID)
	}
}

// cancelShellCommandsInDir stops every composer shell command running in a
// directory that is about to be removed.
func (a *App) cancelShellCommandsInDir(dir string) {
	if a.shellCommands != nil {
		a.shellCommands.CancelDir(dir)
	}
}

func (a *App) stopShellCommands() {
	if a.shellCommands != nil {
		a.shellCommands.Stop()
	}
}

// finishShellCommand turns a settled command into the thread's next user
// message: its output becomes a shell attachment, and the attachment is sent
// alone. The output is removed again only when no message came to hold it:
// a send can fail after its row is written (a session that would not start),
// and that row still shows the output and can be sent again.
func (a *App) finishShellCommand(ctx context.Context, result shellrun.Result) error {
	record, err := a.saveShellOutput(ctx, result)
	if err != nil {
		return fmt.Errorf("save the output: %w", err)
	}
	unlockAdmission, err := a.lockSendAdmission(ctx, result.ThreadID, result.RunID)
	if err == nil {
		_, err = a.sendMessageWithOptions(ctx, result.ThreadID, usermessage.ShellInvocation(result.Command), sendMessageOptions{
			AttachmentIDs:     []string{record.ID},
			SendID:            result.RunID,
			ReconcileBySendID: true,
			QueueIfActive:     true,
			// The command did not come from the draft the composer holds now.
			PreserveDraft: true,
		})
		unlockAdmission()
	}
	if err == nil {
		return nil
	}
	_, recorded, findErr := a.findRecordedSend(result.ThreadID, result.RunID)
	if findErr != nil || recorded {
		// Unknown counts as recorded: a row without its output is worse
		// than an output file nothing shows.
		return errors.Join(err, findErr)
	}
	if deleteErr := a.attachments.Delete(result.ThreadID, record.ID); deleteErr != nil {
		return errors.Join(err, fmt.Errorf("remove the unsent output: %w", deleteErr))
	}
	return err
}

func (a *App) saveShellOutput(ctx context.Context, result shellrun.Result) (store.Attachment, error) {
	unlock, err := a.lockAttachmentUpload(ctx, result.ThreadID)
	if err != nil {
		return store.Attachment{}, err
	}
	defer unlock()
	return a.attachments.SaveShellOutput(result.ThreadID, store.AttachmentShell{
		Command: result.Command, ExitCode: result.ExitCode, DurationMs: result.Duration.Milliseconds(),
		Truncated: result.Truncated, TimedOut: result.TimedOut,
	}, result.OutputPath, time.Now().UnixMilli())
}

// userShellPromptBlock renders a shell output attachment as the text the
// thread's provider reads: the command and its bounded output in the form
// that provider's CLI records a command the person ran.
func userShellPromptBlock(providerName string, record store.Attachment, absolutePath string) (string, error) {
	output, err := attachmentstore.ShellPromptOutput(absolutePath)
	if err != nil {
		return "", err
	}
	command := provider.UserShellCommand{
		Command: record.Shell.Command, Output: output, ExitCode: record.Shell.ExitCode,
		Duration: time.Duration(record.Shell.DurationMs) * time.Millisecond, TimedOut: record.Shell.TimedOut,
	}
	if providerName == string(provider.Codex) {
		return codex.FormatUserShellCommand(command), nil
	}
	return claude.FormatUserShellCommand(command), nil
}

// userShell is the shell a composer command runs under: the person's login
// shell, or /bin/sh when the environment names none.
func userShell() string {
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	return "/bin/sh"
}

// userShellEnvironment is the app's own environment, which already carries the
// login shell's PATH (internal/shellenv), without AppImage launch artifacts
// and without the app's own AO_ variables.
func userShellEnvironment() []string {
	base := appimage.Scrub(os.Environ())
	out := make([]string, 0, len(base))
	for _, entry := range base {
		if !strings.HasPrefix(entry, "AO_") {
			out = append(out, entry)
		}
	}
	return out
}
