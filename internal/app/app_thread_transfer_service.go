package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtransfer"
	"agent-overflow/internal/transferwire"
	"agent-overflow/internal/transport"
)

type appThreadTransfers struct {
	mu   sync.Mutex
	live atomic.Pointer[appTransferService]
}
type appTransferService struct {
	ctx         context.Context
	jobs        *threadtransfer.Jobs
	destination *threadtransfer.Destination
}

func (s *appThreadTransfers) available() error {
	live := s.live.Load()
	if live == nil || live.ctx.Err() != nil {
		return errors.New("Conversation transfers are unavailable while this computer is starting or stopping.")
	}
	return nil
}
func (s *appThreadTransfers) wake(id string) {
	if live := s.live.Load(); live != nil {
		live.jobs.Wake(id)
	}
}
func (s *appThreadTransfers) close() {
	if live := s.live.Load(); live != nil {
		live.jobs.Close()
	}
}

func (a *App) startThreadTransfers() error {
	a.transfers.mu.Lock()
	defer a.transfers.mu.Unlock()
	if a.transfers.live.Load() != nil {
		return nil
	}
	root := filepath.Join(a.configDir, "conversation-transfers")
	source, err := threadtransfer.NewSource(a.store, root, a.snapshotThreadTransfer, a.finalizeTransferredDraft, a.dialComputer)
	if err != nil {
		return err
	}
	destination, err := threadtransfer.NewDestination(a.store, root, appTransferInstaller{a}, a.transfers.wake)
	if err != nil {
		return err
	}
	jobs, err := threadtransfer.NewJobs(a.lifeCtx(), a.store, admittedTransferRunner{a, source}, admittedTransferRunner{a, destination}, transferErrorText,
		a.threadTransferChanged, func(err error) { log.Printf("app: conversation transfer scheduler: %v", err) }, a.transferPendingRetry)
	if err != nil {
		return err
	}
	a.transfers.live.Store(&appTransferService{ctx: a.lifeCtx(), jobs: jobs, destination: destination})
	return nil
}

// threadTransferChanged tells clients a transfer row moved.
func (a *App) threadTransferChanged(row store.ThreadTransfer) {
	a.emit(eventchan.ThreadTransfer, row)
	if err := a.announceTransferredDraft(row); err != nil {
		log.Printf("app: announce transferred draft: %v", err)
	}
}

func (a *App) checkTransferIdle(thread store.Thread) error {
	if thread.Provider != "claude" && thread.Provider != "codex" && thread.Provider != "agy" {
		return errors.New("conversation transfer currently requires a Claude, Codex, or Antigravity conversation")
	}
	if thread.SessionRef == "" && thread.PendingForkRef == "" {
		hasItems, err := a.store.HasItems(thread.ID)
		if err != nil {
			return err
		}
		if hasItems {
			return errors.New("This conversation has history but no saved provider session. Restore its provider session before transferring it.")
		}
	}
	if transferManagedMode(thread.Mode) {
		return errors.New("this conversation belongs to a workflow. Transfer an independent conversation")
	}
	// Moving the conversation ends whatever runs for it here, so every
	// kind of running work refuses but an assistant's watch: the assistant
	// stays on this computer with its watches (errAssistantStays refuses its
	// move), and its copy carries none of them.
	work, err := a.runningWorkIn(workScope{threads: []string{thread.ID}}, workAgentWatch)
	if err != nil {
		return err
	}
	if work.running() {
		switch work.kind {
		case workTurn:
			return errors.New("Let the current turn finish before transferring this conversation.") //nolint:staticcheck // ST1005: the user sees this sentence as written.
		case workAwaitingAnswer, workQueuedMessages:
			return errors.New("Finish or remove queued work before transferring this conversation.") //nolint:staticcheck // ST1005: the user sees this sentence as written.
		case workBackgroundTasks:
			return errors.New("Let background agents and commands finish before transferring this conversation.") //nolint:staticcheck // ST1005: the user sees this sentence as written.
		case workShellCommand:
			return errors.New("Let the shell command finish before transferring this conversation.") //nolint:staticcheck // ST1005: the user sees this sentence as written.
		case workTerminal:
			return errors.New("Close this conversation's terminals before transferring it. Running programs stay on their current computer.") //nolint:staticcheck // ST1005: the user sees this sentence as written.
		case workRemoteWatch:
			// An archived conversation's are finished jobs archive kept, and
			// nothing delivers them until it is unarchived.
			if thread.Archived {
				if unfinished, err := a.store.HasUnfinishedRemoteWatches(thread.ID); err != nil {
					return err
				} else if !unfinished {
					return errors.New(quotedThreadTitle(thread) + " is archived and holds a finished remote job; unarchive it to receive the result first.") //nolint:staticcheck // ST1005: the user sees this sentence as written.
				}
			}
			return errors.New("Let remote commands finish and their completion messages reach this conversation before copying it. Moving the conversation cancels them.") //nolint:staticcheck // ST1005: the user sees this sentence as written.
		case workWorktreeSetup, workRemoteJob, workWorkflow, workProviderUpdate, workAgentWatch:
			fallthrough
		default:
			return fmt.Errorf("This conversation cannot be transferred while %s.", work.kind) //nolint:staticcheck // ST1005: the user sees this sentence as written.
		}
	}
	if a.triage != nil {
		// Recurring jobs do not block: the transfer ends the session and
		// its jobs either way (ListThreadRecurringJobs names them).
		if at, ok := a.triage.PendingScheduledWorkAt(thread.ID); ok && time.Now().Before(at.Add(a.wakeupReapGrace())) {
			return errors.New("cancel the scheduled wakeup or one-time scheduled jobs before transferring this conversation")
		}
	}
	if open, err := a.store.HasOpenThreadRequests(thread.ID); err != nil {
		return err
	} else if open {
		return errors.New("Let this conversation's thread requests finish and their answers arrive before copying it. Moving the conversation cancels them.")
	}
	return nil
}

func (a *App) validateTransferCheckout(ctx context.Context, repository, workspace string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if gitops.SameFilesystemPath(repository, workspace) {
		info, err := os.Stat(workspace)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("The destination project is no longer a directory.")
		}
		return nil
	}
	repoDir, err := a.git.CommonDir(repository)
	if err != nil {
		return err
	}
	workDir, err := a.git.CommonDir(workspace)
	if err != nil {
		return err
	}
	if !gitops.SameFilesystemPath(repoDir, workDir) {
		return errors.New("The destination workspace belongs to another project.")
	}
	return nil
}

// ThreadTransferEndpoints is a bootstrap adapter; methods on this type are
// HTTP capability calls, never accidentally exported as ordinary App RPCs.
func ThreadTransferEndpoints(a *App) transport.ThreadTransferEndpoints {
	return appTransferEndpoints{a}
}

type appTransferEndpoints struct{ app *App }

func (e appTransferEndpoints) destination() (*threadtransfer.Destination, error) {
	if err := e.app.transfers.available(); err != nil {
		return nil, err
	}
	return e.app.transfers.live.Load().destination, nil
}
func (e appTransferEndpoints) Authorize(ctx context.Context, id, grant string) bool {
	d, err := e.destination()
	return err == nil && d.Authorize(ctx, id, grant)
}
func (e appTransferEndpoints) Status(ctx context.Context, id string) (transferwire.State, error) {
	d, err := e.destination()
	if err != nil {
		return transferwire.State{}, err
	}
	return d.Status(ctx, id)
}
func (e appTransferEndpoints) BeginUpload(ctx context.Context, id string, u transferwire.Upload) error {
	release, admitErr := e.app.workAdmission.begin(ctx)
	if admitErr != nil {
		return admitErr
	}
	defer release()

	d, err := e.destination()
	if err != nil {
		return err
	}
	return d.BeginUpload(ctx, id, u)
}
func (e appTransferEndpoints) ReceiveChunk(ctx context.Context, id string, offset, size int64, digest string, input io.Reader) error {
	release, admitErr := e.app.workAdmission.begin(ctx)
	if admitErr != nil {
		return admitErr
	}
	defer release()

	d, err := e.destination()
	if err != nil {
		return err
	}
	return d.ReceiveChunk(ctx, id, offset, size, digest, input)
}
func (e appTransferEndpoints) Prepare(ctx context.Context, id string) error {
	release, admitErr := e.app.workAdmission.begin(ctx)
	if admitErr != nil {
		return admitErr
	}
	defer release()

	d, err := e.destination()
	if err != nil {
		return err
	}
	return d.Prepare(ctx, id)
}
func (e appTransferEndpoints) Activate(ctx context.Context, id string, secret []byte) error {
	release, admitErr := e.app.workAdmission.begin(ctx)
	if admitErr != nil {
		return admitErr
	}
	defer release()

	d, err := e.destination()
	if err != nil {
		return err
	}
	return d.Activate(ctx, id, secret)
}
func (e appTransferEndpoints) Cancel(ctx context.Context, id string, secret []byte) error {
	release, admitErr := e.app.workAdmission.begin(ctx)
	if admitErr != nil {
		return admitErr
	}
	defer release()

	d, err := e.destination()
	if err != nil {
		return err
	}
	return d.Cancel(ctx, id, secret)
}

// transferErrorText is the text a transfer row shows for a failed
// attempt: a public error's reviewed sentence, with its cause in the host
// log, or the error itself.
func transferErrorText(err error) string {
	if _, message, ok := errorsx.PublicDetails(err); ok {
		log.Printf("app: conversation transfer attempt: %v", err)
		return message
	}
	return err.Error()
}

// Transfer attempts are resumable, but their current file/SQLite commit must
// finish before an update snapshots this host. Parked journal work resumes on
// the next boot instead of holding an idle host indefinitely.
type admittedTransferRunner struct {
	app    *App
	runner threadtransfer.Runner
}

func (r admittedTransferRunner) Run(ctx context.Context, id string) (store.ThreadTransfer, error) {
	release, err := r.app.workAdmission.begin(ctx)
	if err != nil {
		return store.ThreadTransfer{}, err
	}
	defer release()
	return r.runner.Run(ctx, id)
}

// quotedThreadTitle names a conversation in a refusal.
func quotedThreadTitle(thread store.Thread) string {
	if thread.Title == "" {
		return "This conversation"
	}
	return `"` + thread.Title + `"`
}
