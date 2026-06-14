package triage

import (
	"encoding/json"

	"agent-overflow/internal/provider"
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
	r.mu.Lock()
	changed := r.upsertTaskCreateLocked(evt.ThreadID, meta.TaskID, meta.Subject)
	steps := r.taskStepsLocked(evt.ThreadID)
	r.mu.Unlock()
	if !changed {
		return nil
	}
	r.projectTodoSnapshot(evt.ThreadID, steps, eventTimestampMillis(evt))
	return nil
}

func (r *Router) handleTaskUpdate(evt provider.ProviderEvent) error {
	var meta provider.TaskUpdateMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil || meta.TaskID == "" {
		return nil
	}
	r.mu.Lock()
	changed := r.applyTaskUpdateLocked(evt.ThreadID, meta)
	steps := r.taskStepsLocked(evt.ThreadID)
	r.mu.Unlock()
	if !changed {
		return nil
	}
	r.projectTodoSnapshot(evt.ThreadID, steps, eventTimestampMillis(evt))
	return nil
}

func (r *Router) upsertTaskCreateLocked(threadID, id, subject string) bool {
	tt := r.tasksByThread[threadID]
	if tt == nil {
		tt = &threadTasks{byID: make(map[string]triageTask)}
		r.tasksByThread[threadID] = tt
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
	tt := r.tasksByThread[threadID]
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
	tt := r.tasksByThread[threadID]
	if tt == nil || len(tt.order) == 0 {
		return nil
	}
	steps := make([]TodoStep, 0, len(tt.order))
	for _, id := range tt.order {
		task, ok := tt.byID[id]
		if !ok {
			continue
		}
		// Bound the model-controlled text the same way the legacy TodoWrite
		// path does in decodeTodoSteps — Subject/Owner come straight from the
		// TaskCreate/TaskUpdate tool input (only TrimSpace'd upstream), so
		// without this an oversized field would blow past the WS-payload /
		// pane-snapshot safety net the maxTodo*Runes caps exist to enforce.
		// Status is a normalized enum and needs no cap. Count is already
		// bounded by maxTasksPerThread.
		steps = append(steps, TodoStep{
			Step:   truncateRunes(task.Subject, maxTodoStepRunes),
			Status: task.Status,
			ID:     truncateRunes(task.ID, maxTodoIDRunes),
			Owner:  truncateRunes(task.Owner, maxTodoOwnerRunes),
		})
	}
	return steps
}
