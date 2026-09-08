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
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
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
	Script         string   `json:"script,omitempty"`
	Interpreter    []string `json:"interpreter,omitempty"`
	Unlimited      bool     `json:"unlimited,omitempty"`
}

type Run func(context.Context, string, []string, io.Writer) (int, error)

type liveJob struct {
	receipt  store.RemoteJob
	tail     *procutil.TailBuffer
	output   *jobLog
	cancel   context.CancelFunc
	finished bool
}

type Manager struct {
	store  *store.Store
	logs   *logStore
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
func New(parent context.Context, st *store.Store, run Run, options ...Options) (*Manager, error) {
	if st == nil || run == nil {
		return nil, errors.New("remote command: store and process runner are required")
	}
	if err := st.RecoverRemoteJobs(); err != nil {
		return nil, err
	}
	var option Options
	if len(options) > 1 {
		return nil, errors.New("remote command: only one options value is allowed")
	}
	if len(options) == 1 {
		option = options[0]
	}
	logs, err := newLogStore(option)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	return &Manager{store: st, logs: logs, ctx: ctx, cancel: cancel, run: run, jobs: make(map[string]*liveJob)}, nil
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

// Validate is shared by the source and destination; malformed requests never
// need a network round trip, and a destination still validates every caller.
func Validate(request Request) error {
	message := ""
	switch {
	case !entityid.Valid(request.ID):
		message = "request_id must be a UUID chosen before calling remote_run. Reuse it only for an identical retry."
	case !entityid.Valid(request.SourceThreadID):
		message = "The source conversation identity is invalid. Reopen the conversation before running a command."
	case request.Unlimited && request.TimeoutSeconds != 0:
		message = "unlimited requires timeout_seconds: 0. Otherwise use a bounded timeout without unlimited."
	case !request.Unlimited && (request.TimeoutSeconds < 1 || request.TimeoutSeconds > MaxTimeoutSeconds):
		message = "timeout_seconds must be between 1 and 604800 (seven days), or use unlimited with timeout_seconds: 0. It limits the job independently of wait_seconds."
	case request.Script != "" && (len(request.Argv) > 0 || len(request.Interpreter) == 0 || request.Interpreter[0] == ""):
		message = "script requires an explicit interpreter argv and cannot be combined with argv. The script file path is appended to the interpreter arguments."
	case request.Script == "" && len(request.Interpreter) > 0:
		message = "interpreter requires script. Use argv for ordinary executable arguments."
	case request.Script == "" && (len(request.Argv) == 0 || request.Argv[0] == ""):
		message = "argv must contain an executable followed by its arguments, or provide script and interpreter. Shell syntax requires an explicit shell such as sh -c."
	case len(request.Script) > 1<<20 || strings.ContainsRune(request.Script, 0):
		message = "script must be at most 1 MiB and cannot contain NUL bytes."
	}
	if message != "" {
		return errorsx.Public("remote_invalid_request", message, nil)
	}
	argv := request.Argv
	if request.Script != "" {
		argv = request.Interpreter
	}
	if len(argv) > 256 {
		return errorsx.Public("remote_invalid_request", "argv or interpreter allows at most 256 entries. Use script for a longer command.", nil)
	}
	bytes := 0
	for _, arg := range argv {
		bytes += len(arg)
		if strings.ContainsRune(arg, 0) {
			return errorsx.Public("remote_invalid_request", "argv and interpreter cannot contain NUL bytes. Remove them before retrying.", nil)
		}
		if bytes > 64<<10 {
			return errorsx.Public("remote_invalid_request", "argv or interpreter exceeds 64 KiB. Use script and interpreter for long commands.", nil)
		}
	}
	return nil
}

func (m *Manager) Start(ownerID, projectID, workspace string, request Request) (store.RemoteJob, error) {
	if err := Validate(request); err != nil {
		return store.RemoteJob{}, err
	}
	request.Argv = append([]string(nil), request.Argv...)
	request.Interpreter = append([]string(nil), request.Interpreter...)
	encoded, _ := json.Marshal(struct {
		Request            Request
		Project, Workspace string
	}{request, projectID, workspace})
	digest := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(digest[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return store.RemoteJob{}, errorsx.Public("remote_shutting_down", "The destination is shutting down. Check the existing receipt after it reconnects, then retry with the same request_id if needed.", nil)
	}
	// Capacity refuses NEW work only. Retrying an accepted command always
	// resolves its receipt, even when all process slots are occupied.
	previous, err := m.store.GetRemoteJob(request.ID)
	if err == nil {
		if previous.OwnerID != ownerID || previous.Fingerprint != fingerprint {
			return store.RemoteJob{}, errorsx.Public("remote_request_conflict", "This request_id already belongs to a different command. Check its status; retry only with the original project, workspace, argv and timeout. Use a new ID only for intentionally separate work.", nil)
		}
		return m.snapshotLocked(previous), nil
	}
	if !errors.Is(err, store.ErrRemoteJobNotFound) {
		return store.RemoteJob{}, err
	}
	if len(m.jobs) >= MaxActive {
		return store.RemoteJob{}, errorsx.Public("remote_capacity", fmt.Sprintf("All %d remote command slots are busy. Wait for a job to finish or cancel one of this conversation’s jobs, then retry with the same request_id.", MaxActive), nil)
	}
	output, err := m.logs.create(request.ID)
	if err != nil {
		return store.RemoteJob{}, errorsx.Public("remote_log_unavailable", "The destination could not reserve command log storage. No command was started. Free disk space or wait for active jobs, then retry with the same request_id.", err)
	}
	receipt, fresh, err := m.store.AcceptRemoteJob(store.RemoteJob{ID: request.ID, OwnerID: ownerID, Fingerprint: fingerprint,
		SourceThreadID: request.SourceThreadID, ProjectID: projectID, Workspace: workspace})
	if err != nil || !fresh {
		m.logs.finish(request.ID)
		_ = os.Remove(filepath.Join(m.logs.options.LogDir, request.ID+".log"))
		return receipt, err
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if request.Unlimited {
		ctx, cancel = context.WithCancel(m.ctx)
	} else {
		ctx, cancel = context.WithTimeout(m.ctx, time.Duration(request.TimeoutSeconds)*time.Second)
	}
	job := &liveJob{receipt: receipt, tail: procutil.NewTailBuffer(store.RemoteJobOutputLimit), output: output, cancel: cancel}
	m.jobs[request.ID] = job
	m.wg.Add(1)
	go m.execute(ctx, job, request)
	return receipt, nil
}

func (m *Manager) execute(ctx context.Context, job *liveJob, request Request) {
	defer m.wg.Done()
	defer job.cancel()
	argv, cleanup, err := m.command(request)
	code := -1
	if err == nil {
		code, err = m.run(ctx, job.receipt.Workspace, argv, io.MultiWriter(job.tail, job.output))
		cleanup()
	}
	m.logs.finish(job.receipt.ID)
	if err != nil {
		log.Printf("remote command %s failed: %v", job.receipt.ID, err)
	}
	m.mu.Lock()
	receipt := job.receipt
	receipt.State, receipt.ExitCode, receipt.FinishedAt = "succeeded", code, time.Now().UnixMilli()
	if err != nil || code != 0 {
		receipt.State = "failed"
	}
	if receipt.State == "failed" {
		switch {
		case errors.Is(err, exec.ErrNotFound):
			receipt.Error = "The executable was not found on the destination's PATH. Install it there or use its absolute path."
		case errors.Is(err, os.ErrNotExist):
			receipt.Error = "The executable or workspace no longer exists on the destination. Check its path and registered project before retrying."
		case errors.Is(err, os.ErrPermission):
			receipt.Error = "The destination denied permission to start the command. Check executable and workspace permissions."
		case code >= 0:
			receipt.Error = fmt.Sprintf("The command exited with code %d. Inspect its output for the failure details.", code)
		default:
			receipt.Error = "The command could not start or was terminated by the destination. Check the executable, workspace availability, and destination logs."
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
	receipt.Output = job.tail.String()
	job.output.mu.Lock()
	receipt.Truncated = job.output.info(receipt.ID).Truncated
	job.output.mu.Unlock()
	job.receipt, job.finished = receipt, true
	m.mu.Unlock()
	// A transient writer failure retains the completed result and its slot.
	// It must never leave a process reported as running or lose its receipt.
	reportedSaveFailure := false
	for {
		err := m.store.FinishRemoteJob(receipt)
		if err == nil {
			m.mu.Lock()
			delete(m.jobs, receipt.ID)
			m.mu.Unlock()
			return
		}
		m.mu.Lock()
		job.receipt.Error = "The command finished, but the destination could not save its result and is retrying. Keep the request_id and check status; do not run the command again."
		m.mu.Unlock()
		if !reportedSaveFailure {
			log.Printf("remote command %s result persistence failed: %v", receipt.ID, err)
			reportedSaveFailure = true
		}
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
		receipt.Output = live.tail.String()
		live.output.mu.Lock()
		receipt.Truncated = live.output.info(receipt.ID).Truncated
		live.output.mu.Unlock()
	}
	return receipt
}

func (m *Manager) Get(ownerID, id string) (store.RemoteJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	receipt, err := m.store.GetRemoteJob(id)
	if errors.Is(err, store.ErrRemoteJobNotFound) {
		return store.RemoteJob{}, errorsx.Public("remote_job_not_found", "No command receipt was found for this request_id on this computer. Check the original computer and request ID. After a lost reply, retry only the identical request with that same ID.", err)
	}
	if err != nil {
		return store.RemoteJob{}, err
	}
	if receipt.OwnerID != ownerID {
		return store.RemoteJob{}, errorsx.Public("remote_wrong_owner", "This command belongs to another paired device. Read or cancel it from the device that submitted it.", nil)
	}
	return m.snapshotLocked(receipt), nil
}

func (m *Manager) Cancel(ownerID, id string) (store.RemoteJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	receipt, err := m.store.GetRemoteJob(id)
	if errors.Is(err, store.ErrRemoteJobNotFound) {
		return store.RemoteJob{}, errorsx.Public("remote_job_not_found", "No command receipt was found for this request_id on this computer. Check the original computer and request ID. After a lost reply, retry only the identical request with that same ID.", err)
	}
	if err != nil {
		return store.RemoteJob{}, err
	}
	if receipt.OwnerID != ownerID {
		return store.RemoteJob{}, errorsx.Public("remote_wrong_owner", "This command belongs to another paired device. Read or cancel it from the device that submitted it.", nil)
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
	if m.logs.temporary {
		_ = os.RemoveAll(m.logs.options.LogDir)
	}
}

// Scripts remain exact bytes in a private temporary file. The caller chooses
// the interpreter and its options; no nested shell interpolation is introduced.
func (m *Manager) command(request Request) ([]string, func(), error) {
	if request.Script == "" {
		return request.Argv, func() {}, nil
	}
	pattern := "script-*"
	switch strings.ToLower(strings.TrimSuffix(filepath.Base(request.Interpreter[0]), ".exe")) {
	case "pwsh", "powershell":
		pattern += ".ps1"
	case "cmd":
		pattern += ".cmd"
	}
	file, err := os.CreateTemp(m.logs.options.LogDir, pattern)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	_, err = file.WriteString(request.Script)
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	argv := append(append([]string(nil), request.Interpreter...), file.Name())
	return argv, cleanup, nil
}
