// Package remotejobs runs bounded, independently cancellable commands on the
// receiving computer. It coordinates processes and receipts, never agent turns.
package remotejobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/store"
)

const MaxActive = 4
const MaxTimeoutSeconds = 7 * 24 * 60 * 60

// Request names exact argv, never shell text to interpolate. Explicitly using
// a shell is possible (e.g. bash -lc), with the same destination authority.
type Request struct {
	ID             string   `json:"id"`
	SourceThreadID string   `json:"sourceThreadId"`
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
}

type Run func(context.Context, string, []string, io.Writer) (int, error)

type liveJob struct {
	receipt  store.RemoteJob
	tail     *procutil.TailBuffer
	cancel   context.CancelFunc
	finished bool
}

type Manager struct {
	store  *store.Store
	ctx    context.Context
	cancel context.CancelFunc
	run    Run
	mu     sync.Mutex
	jobs   map[string]*liveJob
	closed bool
	wg     sync.WaitGroup
}

// New repairs previous accepted work before accepting anything. The owner
// calls Close before closing SQLite. run is mandatory so test fixtures cannot
// accidentally select a real executable from the developer's PATH.
func New(parent context.Context, st *store.Store, run Run) (*Manager, error) {
	if st == nil || run == nil {
		return nil, errors.New("remote command: store and process runner are required")
	}
	if err := st.RecoverRemoteJobs(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	return &Manager{store: st, ctx: ctx, cancel: cancel, run: run, jobs: make(map[string]*liveJob)}, nil
}

// ProcessRunner uses the same process-group and bounded-output primitives as
// workflow commands. Environment belongs to the destination; a requesting
// frontend or agent never supplies credentials or environment overrides.
func ProcessRunner(environment func() []string) Run {
	return func(ctx context.Context, cwd string, argv []string, output io.Writer) (int, error) {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = cwd, output, output
		cmd.Env = environment()
		procutil.ConfigureGroup(cmd)
		err := cmd.Run()
		if cmd.ProcessState != nil {
			return cmd.ProcessState.ExitCode(), err
		}
		return -1, err
	}
}

func validate(request Request) error {
	if !entityid.Valid(request.ID) || !entityid.Valid(request.SourceThreadID) || len(request.Argv) == 0 || len(request.Argv) > 256 ||
		request.Argv[0] == "" || request.TimeoutSeconds < 1 || request.TimeoutSeconds > MaxTimeoutSeconds {
		return errors.New("remote command: provide request and thread UUIDs, argv, and a timeout between 1 second and 7 days")
	}
	bytes := 0
	for _, arg := range request.Argv {
		bytes += len(arg)
		if strings.ContainsRune(arg, 0) || bytes > 64<<10 {
			return errors.New("remote command: argv exceeds 64 KiB or contains a NUL byte")
		}
	}
	return nil
}

func (m *Manager) Start(ownerID, projectID, workspace string, request Request) (store.RemoteJob, error) {
	if err := validate(request); err != nil {
		return store.RemoteJob{}, err
	}
	request.Argv = append([]string(nil), request.Argv...)
	encoded, _ := json.Marshal(struct {
		Request            Request
		Project, Workspace string
	}{request, projectID, workspace})
	digest := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(digest[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return store.RemoteJob{}, errors.New("remote command: this computer is shutting down")
	}
	// Capacity refuses NEW work only. Retrying an accepted command always
	// resolves its receipt, even when all process slots are occupied.
	previous, err := m.store.GetRemoteJob(request.ID)
	if err == nil {
		if previous.OwnerID != ownerID || previous.Fingerprint != fingerprint {
			return store.RemoteJob{}, errors.New("remote command: request ID already belongs to another command")
		}
		return m.snapshotLocked(previous), nil
	}
	if !errors.Is(err, store.ErrRemoteJobNotFound) {
		return store.RemoteJob{}, err
	}
	if len(m.jobs) >= MaxActive {
		return store.RemoteJob{}, fmt.Errorf("remote command: all %d command slots are busy", MaxActive)
	}
	receipt, fresh, err := m.store.AcceptRemoteJob(store.RemoteJob{ID: request.ID, OwnerID: ownerID, Fingerprint: fingerprint,
		SourceThreadID: request.SourceThreadID, ProjectID: projectID, Workspace: workspace})
	if err != nil || !fresh {
		return receipt, err
	}
	ctx, cancel := context.WithTimeout(m.ctx, time.Duration(request.TimeoutSeconds)*time.Second)
	job := &liveJob{receipt: receipt, tail: procutil.NewTailBuffer(store.RemoteJobOutputLimit), cancel: cancel}
	m.jobs[request.ID] = job
	m.wg.Add(1)
	go m.execute(ctx, job, request.Argv)
	return receipt, nil
}

func (m *Manager) execute(ctx context.Context, job *liveJob, argv []string) {
	defer m.wg.Done()
	defer job.cancel()
	code, err := m.run(ctx, job.receipt.Workspace, argv, job.tail)
	m.mu.Lock()
	receipt := job.receipt
	receipt.State, receipt.ExitCode, receipt.FinishedAt = "succeeded", code, time.Now().UnixMilli()
	if err != nil || code != 0 {
		receipt.State = "failed"
	}
	if err != nil {
		receipt.Error = err.Error()
		if len(receipt.Error) > 4096 {
			receipt.Error = receipt.Error[:4096]
		}
	}
	if ctx.Err() != nil {
		receipt.State, receipt.Error = "canceled", "The command was canceled."
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			receipt.State, receipt.Error = "failed", "The command exceeded its time limit."
		}
		if m.ctx.Err() != nil {
			receipt.State, receipt.Error = "interrupted", "The computer stopped before this command finished."
		}
	}
	receipt.Output, receipt.Truncated = job.tail.String(), job.tail.Truncated()
	job.receipt, job.finished = receipt, true
	m.mu.Unlock()
	// A transient writer failure retains the completed result and its slot.
	// It must never leave a process reported as running or lose its receipt.
	for {
		err := m.store.FinishRemoteJob(receipt)
		if err == nil {
			m.mu.Lock()
			delete(m.jobs, receipt.ID)
			m.mu.Unlock()
			return
		}
		m.mu.Lock()
		job.receipt.Error = "Could not save command result: " + err.Error()
		m.mu.Unlock()
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (m *Manager) snapshotLocked(receipt store.RemoteJob) store.RemoteJob {
	if live := m.jobs[receipt.ID]; live != nil {
		receipt = live.receipt
		receipt.Output, receipt.Truncated = live.tail.String(), live.tail.Truncated()
	}
	return receipt
}

func (m *Manager) Get(ownerID, id string) (store.RemoteJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	receipt, err := m.store.GetRemoteJob(id)
	if err != nil {
		return store.RemoteJob{}, err
	}
	if receipt.OwnerID != ownerID {
		return store.RemoteJob{}, errors.New("remote command: this command belongs to another device")
	}
	return m.snapshotLocked(receipt), nil
}

func (m *Manager) Cancel(ownerID, id string) (store.RemoteJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	receipt, err := m.store.GetRemoteJob(id)
	if err != nil {
		return store.RemoteJob{}, err
	}
	if receipt.OwnerID != ownerID {
		return store.RemoteJob{}, errors.New("remote command: this command belongs to another device")
	}
	if live := m.jobs[id]; live != nil && !live.finished {
		live.cancel()
	}
	return m.snapshotLocked(receipt), nil
}

func (m *Manager) HasActive() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.jobs) != 0 }

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
}
