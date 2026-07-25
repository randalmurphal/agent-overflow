package main

import "sync"

// serialQueue runs jobs one at a time, in submission order, on a goroutine that
// exists only while work is pending.
//
// It is the shape every app-side reaction to a workflow engine event needs: the
// engine emits from its command-loop goroutine, so the reaction cannot run
// inline (it reads SQLite, touches git, and can re-enter the engine — the last
// of which would deadlock), and it cannot be a bare `go` either, because two
// transitions of the same run would then race each other's follow-up work.
//
// Wait blocks until the queue drains, which is what lets shutdown finish a
// receipt or a delivery before SQLite closes.
type serialQueue struct {
	mu   sync.Mutex
	wg   sync.WaitGroup
	jobs []func()
	busy bool
}

// Go appends a job and starts the worker if it is not already running.
func (q *serialQueue) Go(job func()) {
	q.mu.Lock()
	q.jobs = append(q.jobs, job)
	if q.busy {
		q.mu.Unlock()
		return
	}
	q.busy = true
	q.wg.Add(1)
	q.mu.Unlock()
	go q.run()
}

func (q *serialQueue) run() {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		if len(q.jobs) == 0 {
			q.busy = false
			q.mu.Unlock()
			return
		}
		job := q.jobs[0]
		q.jobs[0] = nil // Drop the closure's captures with the slot.
		q.jobs = q.jobs[1:]
		q.mu.Unlock()
		job()
	}
}

// Wait blocks until the worker has drained everything submitted before the
// call. Jobs submitted while Wait is blocked are covered too, since the worker
// only reports done with an empty queue.
func (q *serialQueue) Wait() { q.wg.Wait() }
