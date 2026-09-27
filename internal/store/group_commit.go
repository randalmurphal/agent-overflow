package store

import (
	"database/sql"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
)

// groupCommitMax bounds the writes one group commits together, and so how
// long a group holds the writer connection from the writes outside it.
const groupCommitMax = 32

// errWritePanicked fails a write that panicked, and the writes of its
// group that the panic fails with it.
var errWritePanicked = errors.New("panicked")

// groupCommit lets concurrent writes share one transaction.
//
// A commit writes every page its transaction changed to the WAL, so the
// writes of a group write the pages they share once. Writes that arrive
// while a group runs queue up. When the group is done, the write at the
// head of the queue leads the next one: it runs the queued writes, at most
// groupCommitMax, in one transaction, each in its own savepoint, and
// commits them once. There is no timer: a write to an idle store runs at
// once, in a group of one, in a transaction of its own.
//
// A write that fails rolls back to its savepoint and fails alone. A failed
// commit fails every write in the group. A write that ends the transaction
// (SQLite rolls it back itself on some errors) or panics fails the writes
// before it, and the writes after it run in a new transaction.
//
// State a grouped write leaves in the transaction reaches the writes after
// it: defer_foreign_keys, for one, holds until the commit, where a
// violation fails the group. Every write in a group waits for the group's
// commit, so a write's cost delays the other writes too. The store guide
// (internal/store/AGENTS.md, Transactions and writes) states what a
// grouped write must not do.
type groupCommit struct {
	mu    sync.Mutex
	queue []*groupWrite
	// leading is set while a writer leads a group. It is clear only when
	// the queue is empty.
	leading bool
}

type groupWrite struct {
	label, threadID string
	fn              func(tx *sql.Tx) (func(), error)
	// apply is what fn returned. err is nil only once the write has
	// committed, and only then does groupTx run it.
	apply func()
	err   error
	// panicked holds fn's panic until groupTx raises it again on the
	// write's own goroutine.
	panicked *writePanic
	// ready is sent once: true when a leader has finished the write, false
	// when the write heads the queue and its writer leads the next group.
	ready chan bool
}

// writePanic is a grouped write's panic with the stack it was raised on.
type writePanic struct {
	value any
	stack []byte
}

func (p *writePanic) String() string {
	return fmt.Sprintf("%v\n\ngrouped write stack:\n%s", p.value, p.stack)
}

// groupTx runs fn in a transaction it may share with other grouped writes
// (groupCommit). Once that transaction commits with fn's write in it, it
// runs the function fn returned. label and threadID name the write in
// errors. fn may run on another grouped writer's goroutine; a panic in fn
// is raised again on the caller's.
func (s *Store) groupTx(threadID, label string, fn func(tx *sql.Tx) (func(), error)) error {
	w := &groupWrite{label: label, threadID: threadID, fn: fn, ready: make(chan bool, 1)}
	g := &s.groups
	g.mu.Lock()
	g.queue = append(g.queue, w)
	lead := !g.leading
	g.leading = true
	g.mu.Unlock()
	if lead || !<-w.ready {
		s.leadGroup(w)
	}
	if w.panicked != nil {
		panic(w.panicked)
	}
	if w.err != nil {
		return w.err
	}
	if w.apply != nil {
		w.apply()
	}
	return nil
}

// leadGroup commits the group at the head of the queue, which self heads,
// then hands the lead to the next queued write.
func (s *Store) leadGroup(self *groupWrite) {
	g := &s.groups
	var group []*groupWrite
	finished := false
	defer func() {
		if !finished {
			// Begin or a commit panicked, before or after the group was
			// taken. The panic goes on up this goroutine; the other
			// writers get an error.
			if group == nil {
				group = g.take()
			}
			for _, w := range group {
				w.err = fmt.Errorf("store: %s in %s rolled back: its group commit %w", w.label, w.threadID, errWritePanicked)
			}
		}
		for _, w := range group {
			if w != self {
				w.ready <- true
			}
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if len(g.queue) == 0 {
			g.leading = false
			return
		}
		g.queue[0].ready <- false
	}()
	// Take the group once the writer connection is free, so the writes that
	// queue while it is busy commit in this group.
	tx, err := s.db.Begin()
	group = g.take()
	s.commitGroup(tx, err, group)
	finished = true
}

// take removes the next group, at most groupCommitMax writes, from the
// head of the queue.
func (g *groupCommit) take() []*groupWrite {
	g.mu.Lock()
	defer g.mu.Unlock()
	group := slices.Clone(g.queue[:min(len(g.queue), groupCommitMax)])
	g.queue = slices.Delete(g.queue, 0, len(group))
	return group
}

// commitGroup runs group's writes in tx, which Begin returned with
// beginErr, and commits the ones that succeeded, recording each write's
// outcome.
func (s *Store) commitGroup(tx *sql.Tx, beginErr error, group []*groupWrite) {
	if beginErr != nil {
		for _, w := range group {
			w.err = fmt.Errorf("store: begin %s in %s: %w", w.label, w.threadID, beginErr)
		}
		return
	}
	defer tx.Rollback()
	savepoints := len(group) > 1
	var ran []*groupWrite
	for i, w := range group {
		if err := runGroupWrite(tx, w, savepoints); err != nil {
			// The transaction is gone, and the connection must be free
			// before the rest of the group can begin theirs.
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				err = errors.Join(err, rollbackErr)
			}
			for _, done := range ran {
				done.err = fmt.Errorf("store: %s in %s rolled back with %s in %s: %w", done.label, done.threadID, w.label, w.threadID, err)
			}
			if rest := group[i+1:]; len(rest) > 0 {
				tx, err := s.db.Begin()
				s.commitGroup(tx, err, rest)
			}
			return
		}
		if w.err == nil {
			ran = append(ran, w)
		}
	}
	if len(ran) == 0 {
		return
	}
	if err := tx.Commit(); err != nil {
		for _, w := range ran {
			w.err = fmt.Errorf("store: commit %s in %s: %w", w.label, w.threadID, err)
		}
	}
}

// runGroupWrite runs w in tx, in a savepoint when other writes share the
// transaction, and records its outcome. It returns an error, having
// failed w, when the transaction can no longer be used.
func runGroupWrite(tx *sql.Tx, w *groupWrite, savepoint bool) error {
	if savepoint {
		if _, err := tx.Exec(`SAVEPOINT group_write`); err != nil {
			w.err = fmt.Errorf("store: open %s in %s: %w", w.label, w.threadID, err)
			return err
		}
	}
	if w.run(tx); w.panicked != nil {
		return errWritePanicked
	}
	if !savepoint {
		return nil
	}
	if w.err != nil {
		if _, err := tx.Exec(`ROLLBACK TO group_write`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`RELEASE group_write`); err != nil {
		if w.err == nil {
			w.err = fmt.Errorf("store: release %s in %s: %w", w.label, w.threadID, err)
		}
		return err
	}
	return nil
}

// run calls w's fn in tx. A panic in fn fails the write and is held on it
// for groupTx: fn may be running on another writer's goroutine.
func (w *groupWrite) run(tx *sql.Tx) {
	defer func() {
		if v := recover(); v != nil {
			w.apply, w.err = nil, errWritePanicked
			w.panicked = &writePanic{value: v, stack: debug.Stack()}
		}
	}()
	w.apply, w.err = w.fn(tx)
}
