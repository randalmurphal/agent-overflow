package threadtransfer

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"agent-overflow/internal/store"
)

const transferWorkers = 4

type Runner interface {
	Run(context.Context, string) (store.ThreadTransfer, error)
}

// Jobs owns host-lifetime retries for the fixed transfer protocol. SQLite is
// the queue; memory holds only four active IDs and wakeups racing those jobs.
// An empty queue has no polling timer. Incoming waits park until a chunk/proof
// wakes them; restart always rechecks parked work before listening for changes.
type Jobs struct {
	store               *store.Store
	source, destination Runner
	ctx                 context.Context
	cancel              context.CancelFunc
	done                chan struct{}
	wake                chan struct{}
	wg                  sync.WaitGroup
	mu                  sync.Mutex
	active              map[string]bool // true means another wake arrived during this attempt
	retryAfter          time.Time
	describe            func(error) string
	publish             func(store.ThreadTransfer)
	report              func(error)
}

func NewJobs(ctx context.Context, st *store.Store, source, destination Runner, describe func(error) string, publish func(store.ThreadTransfer), report func(error)) (*Jobs, error) {
	if ctx == nil || st == nil || source == nil || destination == nil || describe == nil || publish == nil || report == nil {
		return nil, errors.New("transfer: job lifecycle needs its host dependencies")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := st.WakeThreadTransferJobs(); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	j := &Jobs{store: st, source: source, destination: destination, ctx: life, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), active: make(map[string]bool), describe: describe, publish: publish, report: report}
	go j.loop()
	return j, nil
}

// Wake never runs the installer synchronously. It is safe from a destination
// endpoint that still holds its operation lock. Persisting the wake and tagging
// an active attempt share the finish lock so a late result cannot erase it.
func (j *Jobs) Wake(id string) {
	if j.ctx.Err() != nil {
		return
	}
	j.mu.Lock()
	err := j.store.WakeThreadTransferJob(id)
	if _, running := j.active[id]; running {
		j.active[id] = true
	}
	j.mu.Unlock()
	if err != nil && j.ctx.Err() == nil {
		j.report(err)
	}
	j.signal()
}

func (j *Jobs) signal() {
	select {
	case j.wake <- struct{}{}:
	default:
	}
}

func (j *Jobs) Close() {
	j.cancel()
	<-j.done // No more workers may be added before joining the active ones.
	j.wg.Wait()
}

func (j *Jobs) loop() {
	defer close(j.done)
	for j.ctx.Err() == nil {
		next, err := j.dispatch()
		if err != nil {
			if j.ctx.Err() != nil {
				return
			}
			j.report(err)
			next = time.Now().Add(5 * time.Second)
		}
		var timer *time.Timer
		var elapsed <-chan time.Time
		if !next.IsZero() {
			timer = time.NewTimer(max(time.Until(next), time.Millisecond))
			elapsed = timer.C
		}
		select {
		case <-j.ctx.Done():
		case <-j.wake:
		case <-elapsed:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (j *Jobs) dispatch() (time.Time, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if time.Now().Before(j.retryAfter) {
		return j.retryAfter, nil
	}
	if len(j.active) >= transferWorkers {
		return time.Time{}, nil
	}
	// At most four earliest rows can be running already. Eight candidates
	// suffice to fill all remaining slots without an unbounded exclusion list.
	jobs, err := j.store.NextThreadTransferJobs(transferWorkers * 2)
	if err != nil {
		return time.Time{}, err
	}
	now := time.Now().UnixMilli()
	for _, job := range jobs {
		if _, running := j.active[job.ID]; running {
			continue
		}
		if job.NextAttemptAt == math.MaxInt64 {
			return time.Time{}, nil
		}
		if job.NextAttemptAt > now {
			return time.UnixMilli(job.NextAttemptAt), nil
		}
		j.active[job.ID] = false
		j.wg.Add(1)
		go j.run(job)
		if len(j.active) >= transferWorkers {
			break
		}
	}
	return time.Time{}, nil
}

func (j *Jobs) run(job store.TransferJob) {
	defer j.wg.Done()
	runner := j.source
	if job.Direction == "incoming" {
		runner = j.destination
	}
	_, err := runner.Run(j.ctx, job.ID)
	if j.ctx.Err() != nil {
		return
	} // Startup will recheck this unfinished row.
	message := ""
	retries := 0
	next := int64(math.MaxInt64)
	if err != nil && !errors.Is(err, ErrPending) {
		message = j.describe(err)
		if len(message) > 4096 {
			message = "Transfer paused. Retry after resolving the connection or preparation error."
		}
		retries = min(job.RetryCount+1, 6)
		next = time.Now().Add(time.Duration(1<<retries) * time.Second).UnixMilli()
	} else if job.Direction == "outgoing" && errors.Is(err, ErrPending) {
		next = time.Now().Add(2 * time.Second).UnixMilli()
	}
	j.mu.Lock()
	if j.active[job.ID] {
		next, retries = 0, 0
	}
	persistErr := j.store.FinishThreadTransferAttempt(job.ID, next, retries, message)
	if persistErr != nil {
		j.retryAfter = time.Now().Add(5 * time.Second)
	}
	delete(j.active, job.ID)
	j.mu.Unlock()
	if persistErr != nil {
		j.report(persistErr)
	}
	// Re-read only an active result, so errors/ownership committed during the
	// attempt are reflected. Private recovery blobs never enter a queue scan.
	current, readErr := j.store.GetThreadTransfer(job.ID)
	if readErr != nil {
		j.report(readErr)
	} else if current.Phase != job.Phase || current.Error != job.Error || current.UpdatedAt != job.UpdatedAt {
		j.publish(current)
	}
	j.signal()
}
