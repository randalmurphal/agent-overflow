package triage

import (
	"encoding/json"
	"errors"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

const maxTasksPerThread = 1024

type triageTask struct {
	ID      string
	Subject string
	Status  string
	Owner   string
}

type threadTasks struct {
	byID  map[string]triageTask
	order []string
}

func (r *Router) handleTaskCreate(evt provider.ProviderEvent) error {
	var meta provider.TaskCreateMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil || meta.TaskID == "" {
		return nil
	}
	seedErr := r.seedTasksFromStoredTodo(evt.ThreadID)
	r.mu.Lock()
	changed := r.upsertTaskCreateLocked(evt.ThreadID, meta.TaskID, meta.Subject)
	steps := r.taskStepsLocked(evt.ThreadID)
	r.mu.Unlock()
	if !changed {
		return seedErr
	}
	return errors.Join(seedErr, r.projectTodoSnapshot(evt.ThreadID, steps, eventTimestampMillis(evt)))
}

func (r *Router) handleTaskUpdate(evt provider.ProviderEvent) error {
	var meta provider.TaskUpdateMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil || meta.TaskID == "" {
		return nil
	}
	seedErr := r.seedTasksFromStoredTodo(evt.ThreadID)
	r.mu.Lock()
	changed := r.applyTaskUpdateLocked(evt.ThreadID, meta)
	steps := r.taskStepsLocked(evt.ThreadID)
	r.mu.Unlock()
	if !changed {
		return seedErr
	}
	return errors.Join(seedErr, r.projectTodoSnapshot(evt.ThreadID, steps, eventTimestampMillis(evt)))
}

// seedTasksFromStoredTodo rebuilds a cold thread's Task* correlation map from
// the persisted list (threads.live_todo) before an event is applied.
//
// The map dies with the session (cleanupThread) and the process; the column
// survives both — and so does the PROVIDER's own task list, because a plain
// `claude --resume` keeps its session id and its on-disk task state
// (spike-verified 2026-08-16 on 2.1.219). A resumed session therefore updates
// and deletes ids minted before the restart, and without this seed those
// events would find a nil map, apply to nothing, and never reach
// projectTodoSnapshot — freezing the durable list in a state the provider has
// moved past, with no event that could ever clear it (deletes included).
//
// Only steps with ids seed: a TodoWrite/update_plan list carries none, and
// there is nothing to correlate a Task* event against. The empty map is still
// installed so the miss is not re-read per event, and so a TaskCreate over it
// starts a fresh list exactly as it would over a never-seeded thread.
//
// An ALL-completed stored list also seeds the empty map. The CLI
// (≥2.1.233) deletes a fully-completed list's task files 5s after the
// last completion — the same 5s our reader ages such a list out of view
// with — while its high-water mark keeps later ids monotonic, so those
// ids name tasks the provider no longer has. Seeding them would let the
// next TaskCreate append onto steps the provider already discarded,
// resurrecting a finished list into the fresh one. The narrow cost is a
// session killed inside the CLI's 5s window whose resume then updates a
// completed task: that update applies to nothing and the (hidden,
// finished) projection misses it — until the list's next create
// replaces it wholesale.
//
// The seed is only sound because a stored list implies a resumable session
// whose ids it reflects. The app paths that break that implication — a
// rollback, a provider switch: same thread row, next session from scratch,
// per-session small-integer ids that WOULD collide with a dead list's —
// clear the column through ResetThreadTodo before any Task* event can seed
// from it. A new from-scratch start path must do the same.
//
// A seed read error is returned but does not block the event: an update
// over the uninstalled map applies to nothing (safe — the provider's state
// is untouched and a later event retries the seed), while a create builds a
// fresh list and OVERWRITES the unreadable blob. That overwrite is the heal,
// not the hazard — the realistic read error is a strict-decode refusal of a
// blob some other build wrote, and blocking creates on it would leave the
// blob in place forever, wedging the rail for the thread's lifetime.
//
// This is re-derivation of correlation state from AO's own projection at a
// session boundary, not a cache of store data: the store is read only while
// the map is nil, and the map remains the working truth afterwards.
func (r *Router) seedTasksFromStoredTodo(threadID string) error {
	if r == nil || r.store == nil || threadID == "" {
		return nil
	}
	r.mu.Lock()
	warmState := r.threadStateIfPresent(threadID)
	warm := warmState != nil && warmState.tasks != nil
	r.mu.Unlock()
	if warm {
		return nil
	}
	stored, found, err := r.store.ThreadLiveTodo(threadID)
	if err != nil {
		return fmt.Errorf("seed task state for thread %s: %w", threadID, err)
	}
	tt := &threadTasks{byID: make(map[string]triageTask)}
	if found && !allStepsCompleted(stored.Steps) {
		for _, step := range stored.Steps {
			if step.ID == "" || len(tt.byID) >= maxTasksPerThread {
				continue
			}
			if _, dup := tt.byID[step.ID]; dup {
				continue
			}
			tt.byID[step.ID] = triageTask{
				ID:      step.ID,
				Subject: step.Step,
				Status:  step.Status,
				Owner:   step.Owner,
			}
			tt.order = append(tt.order, step.ID)
		}
	}
	r.mu.Lock()
	// Re-checked under the lock: per-thread events are serialized by their
	// read loop, but the map must never clobber state a racing writer built.
	if st := r.state(threadID); st.tasks == nil {
		st.tasks = tt
	}
	r.mu.Unlock()
	return nil
}

// allStepsCompleted reports whether every step of a stored list is
// completed — the state the CLI deletes a task list in, and therefore
// the state the seed must not resurrect (see seedTasksFromStoredTodo).
func allStepsCompleted(steps []store.ThreadLiveTodoStep) bool {
	for _, step := range steps {
		if step.Status != "completed" {
			return false
		}
	}
	return true
}

func (r *Router) upsertTaskCreateLocked(threadID, id, subject string) bool {
	st := r.state(threadID)
	tt := st.tasks
	if tt == nil {
		tt = &threadTasks{byID: make(map[string]triageTask)}
		st.tasks = tt
	}
	if existing, ok := tt.byID[id]; ok {
		if subject != "" && subject != existing.Subject {
			existing.Subject = subject
			tt.byID[id] = existing
			return true
		}
		return false
	}
	if len(tt.byID) >= maxTasksPerThread {
		return false
	}
	tt.byID[id] = triageTask{
		ID:      id,
		Subject: subject,
		Status:  "pending",
	}
	tt.order = append(tt.order, id)
	return true
}

func (r *Router) applyTaskUpdateLocked(threadID string, meta provider.TaskUpdateMeta) bool {
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return false
	}
	tt := st.tasks
	if tt == nil {
		return false
	}
	task, ok := tt.byID[meta.TaskID]
	if !ok {
		return false
	}
	if meta.Deleted {
		delete(tt.byID, meta.TaskID)
		for i, id := range tt.order {
			if id == meta.TaskID {
				tt.order = append(tt.order[:i], tt.order[i+1:]...)
				break
			}
		}
		return true
	}
	changed := false
	if meta.Subject != "" && meta.Subject != task.Subject {
		task.Subject = meta.Subject
		changed = true
	}
	if meta.Owner != "" && meta.Owner != task.Owner {
		task.Owner = meta.Owner
		changed = true
	}
	if meta.Status != "" && meta.Status != task.Status {
		task.Status = meta.Status
		changed = true
	}
	if changed {
		tt.byID[meta.TaskID] = task
	}
	return changed
}

func (r *Router) taskStepsLocked(threadID string) []TodoStep {
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return nil
	}
	tt := st.tasks
	if tt == nil || len(tt.order) == 0 {
		return nil
	}
	steps := make([]TodoStep, 0, min(len(tt.order), maxTodoSteps))
	for _, id := range tt.order {
		if len(steps) >= maxTodoSteps {
			// The correlation map tracks up to maxTasksPerThread so late
			// updates to any real task still apply, but the PROJECTION is
			// bounded by the same cap as the TodoWrite path — the WS payload,
			// the pane snapshot, and the persisted blob are all sized for
			// maxTodoSteps, and one producer must not ship 4x what the
			// safety net was built for.
			break
		}
		task, ok := tt.byID[id]
		if !ok {
			continue
		}
		// Bound the model-controlled text the same way the legacy TodoWrite
		// path does in decodeTodoSteps — every field comes straight from the
		// TaskCreate/TaskUpdate tool input (only TrimSpace'd upstream), so
		// without this an oversized field would blow past the WS-payload /
		// pane-snapshot safety net the maxTodo*Runes caps exist to enforce.
		// Status included: it is an enum on every known wire, and the cap is
		// what makes that a property of the projection rather than of the
		// provider's good behavior.
		steps = append(steps, TodoStep{
			Step:   truncateRunes(task.Subject, maxTodoStepRunes),
			Status: truncateRunes(task.Status, maxTodoStatusRunes),
			ID:     truncateRunes(task.ID, maxTodoIDRunes),
			Owner:  truncateRunes(task.Owner, maxTodoOwnerRunes),
		})
	}
	return steps
}
