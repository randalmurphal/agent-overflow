package app

import (
	"context"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
)

// The time draft's bound methods: the entries a thread's agent drafted
// (the card of its time_draft row), the person's edit and removal of
// entries, and the person's approval, the one path that sends a draft's
// time (trackerapp.Service.PostTimeEntries). Each change of entries is
// told as tracker:time_entries_updated.

// emitTimeEntries tells every client that entries of a connection's
// author changed on days, drafted by threads among others.
func (a *App) emitTimeEntries(e trackerapp.TimeEntriesEvent) {
	a.emit(eventchan.TrackerTimeEntriesUpdated, e)
}

// ThreadTimeEntries are the time entries that one time_draft call of the
// thread's agent drafted that still exist, with their state now, on every
// connection of this computer: what the call's timeline row shows.
//
//ao:scope tracker:read
func (a *App) ThreadTimeEntries(ctx context.Context, threadID, callID string) ([]trackerapp.TimeEntryView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	out, err := svc.ThreadTimeEntries(threadID, callID)
	return out, trackerError(err)
}

// EditTrackerTimeEntry applies the person's edit to a drafted entry at
// the revision they read; the entry becomes theirs, so a later agent
// draft of its day keeps it. Nothing is sent to the tracker.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) EditTrackerTimeEntry(ctx context.Context, connectionID string, ref trackerapp.TimeEntryRefView, edit trackerapp.TimeEntryEdit) (trackerapp.TimeEntryView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.TimeEntryView{}, err
	}
	out, err := svc.EditTimeEntry(connectionID, ref, edit)
	return out, trackerError(err)
}

// RemoveTrackerTimeEntries deletes the entries named at the revisions read
// that are drafted, of unknown outcome or left out, and answers those it
// deleted. Nothing is sent to the tracker.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) RemoveTrackerTimeEntries(ctx context.Context, connectionID string, refs []trackerapp.TimeEntryRefView) ([]trackerapp.TimeEntryView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	out, err := svc.RemoveTimeEntries(connectionID, refs)
	return out, trackerError(err)
}

// PostTrackerTimeEntries is the person's approval of time entries as they
// read them: it logs them in one batch through the connection's time
// source and answers each entry with the sentence that says what was not
// posted. Its budget is a write's; the worklogs a cut run did not reach
// go back to the draft, and the answer says so.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) PostTrackerTimeEntries(ctx context.Context, connectionID string, in trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.PostTimeView{}, err
	}
	ctx, cancel := a.trackerRun(ctx, "PostTrackerTimeEntries", "Logging the approved time", trackerapp.LostWriteCheck(tracker.StepWorklog))
	defer cancel()
	out, err := svc.PostTimeEntries(ctx, connectionID, in)
	return out, trackerWriteError(ctx, tracker.StepWorklog, err)
}
