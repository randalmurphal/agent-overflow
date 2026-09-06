package app

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

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
	source, err := threadtransfer.NewSource(a.store, root, a.snapshotThreadTransfer)
	if err != nil {
		return err
	}
	destination, err := threadtransfer.NewDestination(a.store, root, appTransferInstaller{a}, a.transfers.wake)
	if err != nil {
		return err
	}
	jobs, err := threadtransfer.NewJobs(a.lifeCtx(), a.store, admittedTransferRunner{a, source}, admittedTransferRunner{a, destination}, func(err error) string { return err.Error() }, func(row store.ThreadTransfer) {
		a.emit(eventchan.ThreadTransfer, row)
	}, func(err error) { log.Printf("app: conversation transfer scheduler: %v", err) })
	if err != nil {
		return err
	}
	a.transfers.live.Store(&appTransferService{ctx: a.lifeCtx(), jobs: jobs, destination: destination})
	return nil
}

func (a *App) checkTransferIdle(thread store.Thread) error {
	if thread.Provider != "claude" && thread.Provider != "codex" {
		return errors.New("Conversation transfer currently requires a Claude or Codex conversation.")
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
	if entry, ok := a.sessionManager().runtime.Get(thread.ID); ok && entry.Liveness != nil && entry.Liveness.ActiveTurns.Load() > 0 {
		return errors.New("Let the current provider turn finish before transferring this conversation.")
	}
	if transferManagedMode(thread.Mode) || thread.DiscussionID != "" || thread.ParentThreadID != "" {
		return errors.New("This conversation belongs to a discussion or workflow. Transfer an independent conversation.")
	}
	if a.terminals != nil && len(a.terminals.List(thread.ID)) > 0 {
		return errors.New("Close this conversation's terminals before transferring it. Running programs stay on their current computer.")
	}
	if _, active, err := a.store.GetActiveTurn(thread.ID); err != nil {
		return err
	} else if active {
		return errors.New("Let the current turn finish before transferring this conversation.")
	}
	if a.triage != nil {
		if a.triage.HasPendingWork(thread.ID) || a.pendingFlushWorkCount(thread.ID) > 0 {
			return errors.New("Finish or remove queued work before transferring this conversation.")
		}
		if at, ok := a.triage.PendingWakeupAt(thread.ID); ok && time.Now().Before(at.Add(wakeupReapGrace)) {
			return errors.New("Cancel the scheduled wakeup before transferring this conversation.")
		}
	}
	running, err := a.store.ListRunningBackgroundToolCalls(thread.ID)
	if err != nil {
		return err
	}
	if len(running) != 0 {
		return errors.New("Let background agents and commands finish before transferring this conversation.")
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
