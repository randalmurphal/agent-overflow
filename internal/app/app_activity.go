package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/activity"
	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/trackertools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// The activity recorder's app side (internal/activity): the screens'
// reports of input, tied to their connections; the recorder's status
// and its change event, for the Time screen; and its start and close.
// Only this computer's person is recorded (ownScreen), and only while a
// Jira connection is on (followJira).

// maxFocusedThreadBytes bounds a report's thread id: ids are UUIDs.
const maxFocusedThreadBytes = 128

// initActivity builds the recorder once the store is open; its flushes
// start with the boot's unattended work (startActivity).
func (a *App) initActivity() {
	cfg := a.activityTiming
	cfg.Changed = func(s activity.Status) { a.emit(eventchan.ActivityRecording, s) }
	a.activity = activity.New(a.store, cfg)
	a.followJira()
}

// tapActivity tells the recorder what it follows from the emit funnel:
// each turn start, which costs the emit a map write (the day is written
// at the recorder's next flush), and each change of the tracker
// connections, which trackerapp announces after it commits. An App built
// without the recorder (a test of another part) records none.
func (a *App) tapActivity(name eventchan.Channel, data any) {
	if a.activity == nil {
		return
	}
	switch name {
	case eventchan.ProviderTurnStarted:
		if event, ok := data.(triage.TurnStartedEvent); ok {
			a.activity.Ran(event.ThreadID, event.StartedAt)
		}
	case eventchan.TrackerConnectionsUpdated:
		a.followJira()
	}
}

// followJira turns the recorder on while a Jira connection is on and off
// while none is. A failed read leaves it as it was and is logged.
func (a *App) followJira() {
	on, err := a.store.JiraConnectionOn()
	if err != nil {
		log.Printf("activity: %v; the recorder stays as it was", err)
		return
	}
	a.activity.SetOn(on)
}

// startActivity starts the recorder's flushes.
func (a *App) startActivity() { a.activity.Start() }

// RecordActivity is input on the calling screen sinceInputMs ago, with
// the thread the screen shows (focused, "" for none) and whether it is in
// the compact layout (phone). A screen calls it at most once a minute,
// for its last input since its last call, and at once when the thread it
// shows changes, on every computer it is attached to. Only a screen of
// this computer's person is recorded (ownScreen): another's call records
// nothing. The span it opens ends with the call's connection.
//
//ao:scope threads:read
//ao:route all
func (a *App) RecordActivity(ctx context.Context, focused string, phone bool, sinceInputMs int64) error {
	conn, err := transport.CallerConnection(ctx)
	if err != nil {
		return err
	}
	if len(focused) > maxFocusedThreadBytes {
		return fmt.Errorf("record activity: a thread id of %d bytes is not a thread's", len(focused))
	}
	if a.activity == nil {
		return errors.New("record activity: the recorder has not started")
	}
	own, err := a.ownScreen(ctx)
	if err != nil {
		return fmt.Errorf("record activity: %w", err)
	}
	if !own {
		return nil
	}
	return a.activity.Report(fmt.Sprintf("%p", conn), focused, phone, time.Duration(sinceInputMs)*time.Millisecond, func(end func()) (func(), error) {
		tie, err := conn.Bind("activity", end)
		return tie.Untie, err
	})
}

// GetActivityRecording is whether this computer is recording the
// person's activity: Since is the earliest activity it has not written
// (Unix ms) while its writes fail, with their error, and 0 while they
// succeed. activity:recording carries each change.
//
//ao:scope tracker:read
//ao:route selected
func (a *App) GetActivityRecording() (activity.Status, error) {
	if a.activity == nil {
		return activity.Status{}, errors.New("activity recording: the recorder has not started")
	}
	return a.activity.Status(), nil
}

// Activity is the recorder's days from from to to with the commits the
// person authored on each, read from the repositories of the projects of
// the range's threads, one git log each, at once; and the recorder's gap.
// A repository whose commits cannot be read is named in CommitsUnread and
// the others answer.
func (h trackerToolsHost) Activity(ctx context.Context, from, to string) (trackertools.ActivityDays, error) {
	a := h.a
	if a.activity == nil {
		return trackertools.ActivityDays{}, errors.New("activity: the recorder has not started")
	}
	first, end, err := activity.DayRange(from, to)
	if err != nil {
		return trackertools.ActivityDays{}, err
	}
	days, err := a.activity.Days(from, to)
	if err != nil {
		return trackertools.ActivityDays{}, err
	}
	out := trackertools.ActivityDays{Gap: a.activity.Status().Since}
	threads := map[string]bool{}
	byDay := map[string]int{}
	for i, d := range days {
		out.Days = append(out.Days, trackertools.ActivityDay{Day: d})
		byDay[d.Day] = i
		for id := range d.Threads {
			if id != "" {
				threads[id] = true
			}
		}
		for _, id := range d.Ran {
			threads[id] = true
		}
	}
	projects, err := a.store.ThreadProjects(slices.Collect(maps.Keys(threads)))
	if err != nil {
		return trackertools.ActivityDays{}, err
	}
	type read struct {
		commits []gitops.AuthoredCommit
		err     error
	}
	reads := make([]read, len(projects))
	var wg sync.WaitGroup
	core := a.gitCore()
	for i, p := range projects {
		wg.Go(func() {
			reads[i].commits, reads[i].err = core.AuthoredCommits(ctx, p.Path, first, end)
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, r := range reads {
		if r.err != nil {
			if ctx.Err() != nil {
				return trackertools.ActivityDays{}, ctx.Err()
			}
			out.CommitsUnread = append(out.CommitsUnread, projects[i].Name+": "+r.err.Error())
			continue
		}
		for _, c := range r.commits {
			if seen[c.Hash] {
				continue
			}
			seen[c.Hash] = true
			day := c.At.Local().Format(time.DateOnly)
			at, ok := byDay[day]
			if !ok {
				at = len(out.Days)
				byDay[day] = at
				out.Days = append(out.Days, trackertools.ActivityDay{Day: activity.Day{Day: day}})
			}
			out.Days[at].Commits = append(out.Days[at].Commits, trackertools.Commit{At: c.At, Subject: c.Subject})
		}
	}
	slices.SortFunc(out.Days, func(x, y trackertools.ActivityDay) int { return strings.Compare(x.Day.Day, y.Day.Day) })
	for i := range out.Days {
		slices.SortStableFunc(out.Days[i].Commits, func(x, y trackertools.Commit) int { return x.At.Compare(y.At) })
	}
	return out, nil
}
