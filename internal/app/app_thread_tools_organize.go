package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// The organizing half of threadtools.App: thread_update and thread_group.
//
// Both go through the bindings the sidebar itself calls, so every change a
// model makes shows up live in every attached client: one thread:updated
// frame per patched thread, a thread-group:updated frame for a group this
// call created, renamed, pinned or deleted, and the archive path's own
// session teardown. Nothing here writes a row the sidebar does not already
// know how to render.
//
// The per-id contract is the spec's: the whole patch is validated against a
// thread before that thread is touched, a refusal names the thread and the
// reason and leaves it untouched, and the other ids in the call still
// apply.

// UpdateThreads applies one organize patch to threads on this computer.
func (t threadToolsApp) UpdateThreads(ctx context.Context, caller threadtools.Caller, call threadtools.UpdateCall) (threadtools.UpdateReport, error) {
	patch := threadapp.OrganizePatch{
		Title:    call.Title,
		Archived: call.Archived,
		Pin:      call.Pin,
		Group:    call.Group,
	}
	// A refusal that belongs to the patch itself is the same answer for
	// every id, so it refuses the call rather than filling fifty rows with
	// one sentence.
	if err := patch.Validate(); err != nil {
		return threadtools.UpdateReport{}, threadToolsOrganizeError("", err)
	}
	if len(call.ThreadIDs) == 0 {
		return threadtools.UpdateReport{}, errorsx.Public(threadtools.CodeInvalidRequest, "Name at least one thread to update.", nil)
	}

	report := threadtools.UpdateReport{Results: make([]threadtools.ThreadUpdateResult, 0, len(call.ThreadIDs))}
	for _, threadID := range call.ThreadIDs {
		report.Results = append(report.Results, t.updateOneThread(ctx, caller, threadID, patch))
	}
	return report, nil
}

// updateOneThread applies the patch to one thread and reports that thread's
// own outcome. It never returns an error: one thread's refusal is a row in
// the result, and the ids behind it are still applied.
func (t threadToolsApp) updateOneThread(ctx context.Context, caller threadtools.Caller, threadID string, patch threadapp.OrganizePatch) threadtools.ThreadUpdateResult {
	result := threadtools.ThreadUpdateResult{ThreadID: threadID}
	// Resolution already named a thread on this computer, but a thread can
	// be deleted or handed to another computer between then and now, so the
	// row is rechecked here rather than assumed.
	thread, err := t.localThread(threadID)
	if err != nil {
		result.Error, result.ErrorCode = organizeRefusal(threadID, err)
		return result
	}
	result.Title = thread.Title
	if threadID == caller.ThreadID && patch.Archived != nil && *patch.Archived {
		result.Error = "A thread cannot archive itself. Archive it from another thread, or leave it to the user."
		result.ErrorCode = threadtools.CodeIsCaller
		return result
	}

	applied, err := t.app.applyThreadOrganizePatch(ctx, threadID, patch)
	if err != nil {
		result.Error, result.ErrorCode = organizeRefusal(threadID, err)
		return result
	}
	if applied.Thread.Title != "" {
		result.Title = applied.Thread.Title
	}
	result.Updated = true
	return result
}

// applyThreadOrganizePatch is the write itself, with the locks, the events
// and the archive side effects the sidebar's own bindings carry.
//
// Archiving releases the thread's provider session (app_thread_archive.go
// says why), and that release is serialized by the ACTION lock, so a patch
// that archives takes action then mutation, the order transfer takes, and
// never the reverse. A patch that does not archive takes the mutation lock
// alone, exactly as the pin and rename bindings do.
func (a *App) applyThreadOrganizePatch(ctx context.Context, threadID string, patch threadapp.OrganizePatch) (threadapp.OrganizeResult, error) {
	result, err := a.applyThreadOrganizePatchLocked(ctx, threadID, patch)
	if err != nil {
		return threadapp.OrganizeResult{}, err
	}
	if result.ArchivedChanged && result.Thread.Archived {
		// The same cleanup the archive binding runs, through the same door:
		// the requests this thread is waiting on end and its wakes are
		// disarmed, then the commands it started elsewhere are cancelled.
		// The archive itself is already durable, so a failure to reach the
		// other computer is reported here rather than turned into a refusal
		// of a change that landed; the destination stops the work itself
		// after the owner grace.
		if err := a.stopArchivedThreadWork(ctx, threadID); err != nil {
			log.Printf("thread tools: archive thread %s: cancel remote commands: %v", threadID, err)
		}
	}
	return result, nil
}

func (a *App) applyThreadOrganizePatchLocked(ctx context.Context, threadID string, patch threadapp.OrganizePatch) (threadapp.OrganizeResult, error) {
	archiving := patch.Archived != nil && *patch.Archived
	// Stamped before the locks, for the same reason ArchiveThread stamps it
	// there: waiting for the lock is exactly the window in which this
	// archive can go stale, and stopArchivedThreadSession re-checks against
	// this instant.
	requestedAt := time.Now().UnixMilli()
	if archiving {
		unlockAction := a.threadLocks().Lock(threadID)
		defer unlockAction()
	}
	unlock, err := a.threadApplication().LockMutable(ctx, threadID)
	if err != nil {
		return threadapp.OrganizeResult{}, err
	}
	defer unlock()

	result, err := a.threadApplication().ApplyOrganizePatch(threadID, patch)
	if err != nil {
		return threadapp.OrganizeResult{}, err
	}
	// The group frame goes first: a client that receives the thread's new
	// groupId before it has the group row has nowhere to render the thread.
	if result.CreatedGroup != nil {
		a.emitThreadGroup("create", *result.CreatedGroup)
	}
	a.broadcastThreadRowIfChanged(organizeThreadAction(result), result.Thread, result.Changed)
	// The discussion children a group move carried, as SetThreadGroup's own
	// binding announces them.
	for _, carried := range result.Carried {
		a.broadcastThreadRow(triage.ThreadActionFull, carried)
	}
	if result.ArchivedChanged && result.Thread.Archived {
		a.stopArchivedThreadSession(threadID, requestedAt)
	}
	return result, nil
}

// organizeThreadAction picks the sidebar frame this patch produced. An
// archive flip is what adds or removes the row from the active sidebar, so
// it is announced as such; anything else is a whole-row update of a row
// that is already there.
func organizeThreadAction(result threadapp.OrganizeResult) string {
	if !result.ArchivedChanged {
		return triage.ThreadActionFull
	}
	if result.Thread.Archived {
		return triage.ThreadActionUnlisted
	}
	return triage.ThreadActionListed
}

// UpdateGroup renames, deletes or pins one group of this computer.
func (t threadToolsApp) UpdateGroup(_ context.Context, caller threadtools.Caller, call threadtools.GroupCall) (threadtools.GroupReport, error) {
	if err := validateGroupCall(call); err != nil {
		return threadtools.GroupReport{}, err
	}
	group, err := t.resolveToolGroup(caller, call)
	if err != nil {
		return threadtools.GroupReport{}, err
	}
	report := threadtools.GroupReport{GroupID: group.ID, Group: group.Name, ProjectID: group.ProjectID}

	switch {
	case call.Delete:
		// Counted before the delete: afterwards the members carry no group
		// and there is nothing left to count.
		ungrouped, err := t.app.threadApplication().GroupMemberCount(group)
		if err != nil {
			return threadtools.GroupReport{}, threadToolsOrganizeError(group.ID, err)
		}
		if err := t.app.DeleteThreadGroup(group.ID); err != nil {
			return threadtools.GroupReport{}, threadToolsOrganizeError(group.ID, err)
		}
		report.Action, report.Ungrouped = "deleted", ungrouped
	case call.Rename != "":
		renamed, err := t.app.RenameThreadGroup(group.ID, call.Rename)
		if err != nil {
			return threadtools.GroupReport{}, threadToolsOrganizeError(group.ID, err)
		}
		report.Group, report.Action = renamed.Name, "renamed"
	default:
		pinned, err := t.applyGroupPin(group, call.Pin)
		if err != nil {
			return threadtools.GroupReport{}, threadToolsOrganizeError(group.ID, err)
		}
		report.Group, report.Action, report.Pin = pinned.Name, "pinned", call.Pin
	}
	return report, nil
}

// validateGroupCall repeats the tool layer's shape checks inside the API,
// so a caller that reaches this method by another route cannot skip them.
func validateGroupCall(call threadtools.GroupCall) error {
	named := 0
	if strings.TrimSpace(call.Group) != "" {
		named++
	}
	if strings.TrimSpace(call.GroupID) != "" {
		named++
	}
	if named != 1 {
		return errorsx.Public(threadtools.CodeInvalidRequest, "Name the group with group plus project_id, or with group_id, and not both.", nil)
	}
	actions := 0
	if strings.TrimSpace(call.Rename) != "" {
		actions++
	}
	if strings.TrimSpace(call.Pin) != "" {
		actions++
	}
	if call.Delete {
		actions++
	}
	if actions != 1 {
		return errorsx.Public(threadtools.CodeInvalidRequest, "Pass exactly one of rename, pin or delete.", nil)
	}
	switch call.Pin {
	case "", threadtools.PinFront, threadtools.PinBack, threadtools.PinNone:
	default:
		return errorsx.Public(threadtools.CodeInvalidRequest, "pin must be front, back or none.", nil)
	}
	return nil
}

// resolveToolGroup finds the group a thread_group call names: by id, or by
// name inside one project. A named group with no project_id resolves in the
// CALLER's own project, which is the project the model is working in and
// the only one it can mean without saying so.
func (t threadToolsApp) resolveToolGroup(caller threadtools.Caller, call threadtools.GroupCall) (store.ThreadGroup, error) {
	if id := strings.TrimSpace(call.GroupID); id != "" {
		group, err := t.app.store.GetThreadGroup(id)
		if err != nil {
			return store.ThreadGroup{}, errorsx.Public(threadtools.CodeNotFound,
				fmt.Sprintf("No thread group %s on this computer. thread_options lists the groups of each project.", id), err)
		}
		return group, nil
	}

	name := strings.TrimSpace(call.Group)
	projectID := strings.TrimSpace(call.ProjectID)
	if projectID == "" {
		thread, err := t.localThread(caller.ThreadID)
		if err != nil {
			return store.ThreadGroup{}, errorsx.Public(threadtools.CodeInvalidRequest,
				"Name the project the group is in with project_id: a group belongs to one project.", err)
		}
		projectID = thread.ProjectID
	}
	if projectID == "" {
		return store.ThreadGroup{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"Name the project the group is in with project_id: a group belongs to one project.", nil)
	}
	group, found, err := t.app.threadApplication().FindGroup(projectID, name)
	if err != nil {
		return store.ThreadGroup{}, threadToolsOrganizeError("", err)
	}
	if !found {
		return store.ThreadGroup{}, errorsx.Public(threadtools.CodeNotFound,
			fmt.Sprintf("No group named %q in project %s. thread_options lists the groups of each project; thread_update with group creates one.", name, projectID), nil)
	}
	return group, nil
}

// applyGroupPin writes one group's pin tier through the same bindings the
// sidebar's pin affordance calls, including its two-step for the back
// burner: a group is pinned first and moved between burners second, because
// the store refuses a burner on an unpinned row.
func (t threadToolsApp) applyGroupPin(group store.ThreadGroup, tier string) (store.ThreadGroup, error) {
	switch tier {
	case threadtools.PinNone:
		return t.app.UnpinThreadGroup(group.ID)
	case threadtools.PinFront:
		return t.app.PinThreadGroup(group.ID)
	case threadtools.PinBack:
		if group.PinnedAt == nil {
			if _, err := t.app.PinThreadGroup(group.ID); err != nil {
				return store.ThreadGroup{}, err
			}
		}
		return t.app.SetThreadGroupPinGroup(group.ID, store.PinGroupBack)
	}
	return store.ThreadGroup{}, errorsx.Public(threadtools.CodeInvalidRequest, "pin must be front, back or none.", nil)
}

// organizeRefusal renders one thread's refusal for its result row: a code
// the model can act on and prose that says what to do instead.
func organizeRefusal(threadID string, err error) (message, code string) {
	public := threadToolsOrganizeError(threadID, err)
	code, message, _ = errorsx.PublicDetails(public)
	return message, code
}

// threadToolsOrganizeError turns a store or service failure into the public
// refusal the model reads. Everything this package knows how to explain has
// a code; an unexpected failure keeps its cause in the host log and reaches
// the model as prose with no code, because inventing one would tell the
// model a retry rule that does not exist.
func threadToolsOrganizeError(threadID string, err error) error {
	if err == nil {
		return nil
	}
	if _, _, ok := errorsx.PublicDetails(err); ok {
		return err
	}
	var moved *store.ThreadTransferError
	switch {
	case errors.Is(err, store.ErrThreadGrouped), errors.Is(err, threadapp.ErrOrganizeGroupAndPin):
		return errorsx.Public(threadtools.CodeGrouped,
			"A grouped thread cannot carry its own pin, because its group carries it. Ungroup it first, or pin the group with thread_group.", err)
	case errors.Is(err, threadapp.ErrEmptyThreadTitle):
		return errorsx.Public(threadtools.CodeInvalidRequest,
			"A title is trimmed, and an empty title is refused. Omit title to leave it alone.", err)
	case errors.Is(err, threadapp.ErrThreadHasNoProject):
		return errorsx.Public(threadtools.CodeInvalidRequest,
			"That thread belongs to no project, and a group belongs to one project, so it cannot join a group.", err)
	case errors.Is(err, store.ErrThreadNotRoot):
		return errorsx.Public(threadtools.CodeInvalidRequest,
			"That thread is a discussion reply and travels with its discussion. Group the top-level thread instead.", err)
	case errors.Is(err, store.ErrThreadGroupGone):
		return errorsx.Public(threadtools.CodeNotFound,
			"That group is no longer in this project. Read the groups again with thread_options.", err)
	case errors.Is(err, store.ErrEmptyThreadGroupName):
		return errorsx.Public(threadtools.CodeInvalidRequest, "A group name is trimmed, and a blank one is refused.", err)
	case errors.Is(err, store.ErrThreadGone), errors.Is(err, sql.ErrNoRows), errors.As(err, &moved):
		return threadToolsNotFound(threadID)
	}
	log.Printf("thread tools: organize %s: %v", threadID, err)
	return errorsx.Public("", "That change could not be applied on this computer, and nothing was changed.", err)
}
