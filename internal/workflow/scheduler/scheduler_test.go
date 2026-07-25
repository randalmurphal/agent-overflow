package scheduler

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// baseTime is 02:30 UTC, chosen so the fixtures below have an obvious next
// occurrence (02:45 for a quarter-hour schedule, 03:00 for a daily one).
var baseTime = time.Date(2026, 7, 24, 2, 30, 0, 0, time.UTC)

func TestCronFiresInScheduleOrder(t *testing.T) {
	daily := cronAutomation("daily", "project-a", "0 3 * * *")
	quarter := cronAutomation("quarter", "project-a", "*/15 * * * *")
	h := newHarness(t, baseTime, daily, quarter)

	// 02:45 belongs to the quarter-hour schedule alone.
	h.advance(15 * time.Minute)
	if got := h.startedAutomations(); !reflect.DeepEqual(got, []string{"quarter"}) {
		t.Fatalf("starts after 02:45 = %v, want [quarter]", got)
	}

	// 03:00 is due for both. Each fires exactly once — a fired occurrence is
	// dropped and recomputed from the fire time — and equal occurrences keep the
	// automation order the store returned, which is creation order.
	h.advance(15 * time.Minute)
	if got := h.startedAutomations(); !reflect.DeepEqual(got, []string{"quarter", "daily", "quarter"}) {
		t.Fatalf("starts after 03:00 = %v, want [quarter daily quarter]", got)
	}

	fires := h.store.snapshotFires()
	if len(fires) != 3 {
		t.Fatalf("fire records = %v, want three", fires)
	}
	for _, fire := range fires {
		if fire.itemID == "" || fire.at == 0 {
			t.Fatalf("fire record is incomplete: %#v", fire)
		}
	}
	if skips := h.store.snapshotSkips(); len(skips) != 0 {
		t.Fatalf("skips = %v, want none", skips)
	}
}

func TestCronGoalAndReservedSeeds(t *testing.T) {
	automation := cronAutomation("nightly", "project-a", "*/15 * * * *")
	automation.Name = "Nightly audit"
	automation.Notes = "the flaky suite is still quarantined"
	// A stored seed under a reserved name can only arrive by hand-editing the
	// row (creation refuses it); the injected value still wins, because reserved
	// bindings are applied last.
	automation.Seeds = json.RawMessage(`{"scope":"api","trigger":"stale"}`)
	h := newHarness(t, baseTime, automation)

	h.advance(15 * time.Minute)
	starts := h.snapshotStarts()
	if len(starts) != 1 {
		t.Fatalf("starts = %v, want one", starts)
	}
	started := starts[0]
	if started.goal != "Nightly audit (cron 2026-07-24T02:45:00Z)" {
		t.Fatalf("goal = %q", started.goal)
	}
	if started.seeds["scope"] != "api" {
		t.Fatalf("stored seeds were dropped: %#v", started.seeds)
	}
	if started.seeds[JobNotesVariable] != "the flaky suite is still quarantined" {
		t.Fatalf("job notes seed = %#v", started.seeds[JobNotesVariable])
	}
	trigger, ok := started.seeds[TriggerVariable].(map[string]any)
	if !ok {
		t.Fatalf("trigger seed = %#v, want an object", started.seeds[TriggerVariable])
	}
	if trigger["kind"] != string(KindCron) {
		t.Fatalf("trigger.kind = %#v", trigger["kind"])
	}
	scheduled, ok := trigger["scheduled-for"].(float64)
	if !ok || int64(scheduled) != baseTime.Add(15*time.Minute).UnixMilli() {
		t.Fatalf("trigger.scheduled-for = %#v", trigger["scheduled-for"])
	}
	if _, ok := trigger["fired-at"].(float64); !ok {
		t.Fatalf("trigger.fired-at = %#v", trigger["fired-at"])
	}
}

func TestCronRecomputesAfterCRUD(t *testing.T) {
	h := newHarness(t, baseTime)

	// Nothing is scheduled yet, so the loop is parked on its idle wait.
	h.advance(15 * time.Minute)
	if got := h.startedAutomations(); len(got) != 0 {
		t.Fatalf("starts with no automations = %v", got)
	}

	h.store.put(cronAutomation("added", "project-a", "*/15 * * * *"))
	h.sync() // The CRUD notification every automation write makes.
	h.advance(15 * time.Minute)
	if got := h.startedAutomations(); !reflect.DeepEqual(got, []string{"added"}) {
		t.Fatalf("starts after adding an automation = %v, want [added]", got)
	}

	// Disabling it removes it from the schedule on the next recompute.
	disabled := cronAutomation("added", "project-a", "*/15 * * * *")
	disabled.Enabled = false
	h.store.put(disabled)
	h.sync()
	h.advance(15 * time.Minute)
	if got := h.startedAutomations(); !reflect.DeepEqual(got, []string{"added"}) {
		t.Fatalf("starts after disabling = %v, want no new start", got)
	}
}

func TestBootDoesNotReplayMissedFires(t *testing.T) {
	// 04:00: today's 03:00 occurrence is in the past. The app was closed for it,
	// so it is neither replayed nor recorded as a skip.
	bootTime := time.Date(2026, 7, 24, 4, 0, 0, 0, time.UTC)
	h := newHarness(t, bootTime, cronAutomation("daily", "project-a", "0 3 * * *"))

	if got := h.startedAutomations(); len(got) != 0 {
		t.Fatalf("starts at boot = %v, want none", got)
	}
	if skips := h.store.snapshotSkips(); len(skips) != 0 {
		t.Fatalf("skips at boot = %v, want none", skips)
	}

	// The next occurrence is tomorrow's, and it does fire.
	h.advance(23 * time.Hour)
	if got := h.startedAutomations(); !reflect.DeepEqual(got, []string{"daily"}) {
		t.Fatalf("starts after the next occurrence = %v, want [daily]", got)
	}
}

func TestEventTriggerMatching(t *testing.T) {
	h := newHarness(t, baseTime,
		eventAutomation("any-done", "project-a", EventItemDone, ""),
		eventAutomation("filtered", "project-a", EventItemDone, "audit"),
		eventAutomation("failures", "project-a", EventItemFailed, ""),
		eventAutomation("other-project", "project-b", EventItemDone, ""),
	)

	cases := []struct {
		name  string
		event ItemEvent
		want  []string
	}{
		{
			name:  "unfiltered kind match in the same project",
			event: ItemEvent{ProjectID: "project-a", ItemID: "run-1", WorkflowID: "release", State: "done", Source: "manual"},
			want:  []string{"any-done"},
		},
		{
			name:  "workflow filter narrows to its own workflow",
			event: ItemEvent{ProjectID: "project-a", ItemID: "run-2", WorkflowID: "audit", State: "done", Source: "manual"},
			want:  []string{"any-done", "filtered"},
		},
		{
			name:  "another project never matches",
			event: ItemEvent{ProjectID: "project-c", ItemID: "run-3", WorkflowID: "audit", State: "done", Source: "manual"},
			want:  nil,
		},
		{
			name:  "a called run's transition is its tree's internal business",
			event: ItemEvent{ProjectID: "project-a", ItemID: "run-4", WorkflowID: "audit", State: "done", Source: "call", ParentItemID: "run-2"},
			want:  nil,
		},
		{
			name:  "cancelled is outside the closed event set",
			event: ItemEvent{ProjectID: "project-a", ItemID: "run-5", WorkflowID: "audit", State: "cancelled", Source: "manual"},
			want:  nil,
		},
		{
			name:  "a park is its own event kind",
			event: ItemEvent{ProjectID: "project-a", ItemID: "run-6", WorkflowID: "audit", State: "needs-human", Reason: "question", Source: "manual"},
			want:  nil,
		},
		{
			name:  "failures reach only the failure trigger",
			event: ItemEvent{ProjectID: "project-a", ItemID: "run-7", WorkflowID: "audit", State: "failed", Reason: "agent-error", Source: "manual"},
			want:  []string{"failures"},
		},
	}
	seen := 0
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := h.scheduler.NotifyItemEvent(testCase.event); err != nil {
				t.Fatalf("NotifyItemEvent() error = %v", err)
			}
			starts := h.startedAutomations()
			got := starts[seen:]
			seen = len(starts)
			if len(got) != len(testCase.want) {
				t.Fatalf("starts = %v, want %v", got, testCase.want)
			}
			for index, want := range testCase.want {
				if got[index] != want {
					t.Fatalf("starts = %v, want %v", got, testCase.want)
				}
			}
		})
	}

	// The event context reaches the run as the reserved trigger variable.
	starts := h.snapshotStarts()
	first := starts[0]
	trigger, ok := first.seeds[TriggerVariable].(map[string]any)
	if !ok {
		t.Fatalf("trigger seed = %#v", first.seeds[TriggerVariable])
	}
	event, ok := trigger["event"].(map[string]any)
	if !ok {
		t.Fatalf("trigger.event = %#v", trigger["event"])
	}
	if event["item-id"] != "run-1" || event["state"] != "done" || event["kind"] != string(EventItemDone) {
		t.Fatalf("trigger.event = %#v", event)
	}
	if !strings.Contains(first.goal, "event item-done from run run-1") {
		t.Fatalf("goal = %q", first.goal)
	}
}

func TestSelfChainIsSkipped(t *testing.T) {
	h := newHarness(t, baseTime,
		eventAutomation("loop", "project-a", EventItemDone, ""),
		eventAutomation("neighbour", "project-a", EventItemDone, ""),
	)

	// The completing run is one this automation itself started: chaining it back
	// into itself is always an authoring accident. A second automation reacting
	// to the same run is a legal chain and still fires.
	if err := h.scheduler.NotifyItemEvent(ItemEvent{
		ProjectID: "project-a", ItemID: "run-9", WorkflowID: "audit", State: "done",
		Source: Source, SourceRef: "loop",
	}); err != nil {
		t.Fatalf("NotifyItemEvent() error = %v", err)
	}

	if got := h.startedAutomations(); !reflect.DeepEqual(got, []string{"neighbour"}) {
		t.Fatalf("starts = %v, want [neighbour]", got)
	}
	skips := h.store.snapshotSkips()
	if len(skips) != 1 || skips[0].automationID != "loop" || !strings.HasPrefix(skips[0].reason, "self-chain:") {
		t.Fatalf("skips = %#v, want one self-chain skip for loop", skips)
	}
}

func TestSkipIfRunningRecordsTheBlockingRun(t *testing.T) {
	h := newHarness(t, baseTime, cronAutomation("nightly", "project-a", "*/15 * * * *"))
	// A parked run is unfinished work: overlapping through a park is still
	// overlap, and the recorded skip is what surfaces a starving park.
	h.store.setActive("nightly", store.AutomationRun{
		ItemID: "run-parked", State: "needs-human", Reason: "question",
	})

	h.advance(15 * time.Minute)

	if got := h.startedAutomations(); len(got) != 0 {
		t.Fatalf("starts = %v, want none", got)
	}
	skips := h.store.snapshotSkips()
	if len(skips) != 1 {
		t.Fatalf("skips = %#v, want one", skips)
	}
	if skips[0].reason != "run run-parked is still needs-human(question)" {
		t.Fatalf("skip reason = %q", skips[0].reason)
	}
	if skips[0].at != baseTime.Add(15*time.Minute).UnixMilli() {
		t.Fatalf("skip at = %d", skips[0].at)
	}
}

func TestConditionSkips(t *testing.T) {
	cases := []struct {
		name      string
		condition string
		notes     string
		wantSkip  string
	}{
		{
			name:      "false condition",
			condition: `{"eq":{"ref":"trigger.kind","value":"event"}}`,
			wantSkip:  "condition false",
		},
		{
			name:      "unevaluable condition",
			condition: `{"gt":{"ref":"job-notes","value":1}}`,
			notes:     "not a number",
			wantSkip:  "condition error:",
		},
		{
			name:      "malformed condition",
			condition: `{"eq":{"ref":"trigger.kind","value":"cron"},"exists":"trigger"}`,
			wantSkip:  "condition error:",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			automation := cronAutomation("nightly", "project-a", "*/15 * * * *")
			automation.Condition = json.RawMessage(testCase.condition)
			automation.Notes = testCase.notes
			h := newHarness(t, baseTime, automation)

			h.advance(15 * time.Minute)

			if got := h.startedAutomations(); len(got) != 0 {
				t.Fatalf("starts = %v, want none", got)
			}
			skips := h.store.snapshotSkips()
			if len(skips) != 1 || !strings.HasPrefix(skips[0].reason, testCase.wantSkip) {
				t.Fatalf("skips = %#v, want one starting with %q", skips, testCase.wantSkip)
			}
		})
	}
}

func TestConditionTrueStarts(t *testing.T) {
	automation := cronAutomation("nightly", "project-a", "*/15 * * * *")
	automation.Condition = json.RawMessage(`{"eq":{"ref":"trigger.kind","value":"cron"}}`)
	h := newHarness(t, baseTime, automation)

	h.advance(15 * time.Minute)

	if got := h.startedAutomations(); !reflect.DeepEqual(got, []string{"nightly"}) {
		t.Fatalf("starts = %v, want [nightly]", got)
	}
	if skips := h.store.snapshotSkips(); len(skips) != 0 {
		t.Fatalf("skips = %#v, want none", skips)
	}
}

// A fire reads the automation when it fires, not when it was armed. Continuity
// notes are the whole reason: a human (or the previous run) writes them between
// two ticks, and the next run has to read what was written.
func TestFireReadsTheAutomationAtFireTime(t *testing.T) {
	automation := cronAutomation("nightly", "project-a", "*/15 * * * *")
	automation.Notes = "stale"
	h := newHarness(t, baseTime, automation)

	updated := automation
	updated.Notes = "migration 42 is half applied"
	updated.Seeds = json.RawMessage(`{"goal":"finish it"}`)
	h.store.put(updated)

	h.advance(15 * time.Minute)

	starts := h.snapshotStarts()
	if len(starts) != 1 {
		t.Fatalf("starts = %#v, want one", starts)
	}
	if got := starts[0].seeds[JobNotesVariable]; got != "migration 42 is half applied" {
		t.Fatalf("job notes = %v, want the value written after the occurrence was armed", got)
	}
	if got := starts[0].seeds["goal"]; got != "finish it" {
		t.Fatalf("goal = %v, want the seed written after the occurrence was armed", got)
	}
}

// What a fire does when the row it was armed for no longer says what it did.
// Asserted on the fire itself rather than through the loop: whether the loop
// even reaches a fire for a row that was just disabled depends on which of the
// two re-reads wins, and the gate has to hold either way.
func TestFireLoadGates(t *testing.T) {
	cases := []struct {
		name    string
		manual  bool
		setup   func(*fakeStore)
		wantErr string
	}{
		{
			// Deleted while its occurrence was in flight: nothing to run, and no
			// row left to record a skip on.
			name:  "deleted",
			setup: func(s *fakeStore) { s.remove("nightly") },
		},
		{
			// Disabled while its occurrence was in flight. Not a skip: a disabled
			// automation has no schedule to have skipped.
			name: "disabled",
			setup: func(s *fakeStore) {
				disabled := cronAutomation("nightly", "project-a", "*/15 * * * *")
				disabled.Enabled = false
				s.put(disabled)
			},
		},
		{
			// Not knowing what an automation says is not permission to run it —
			// and not a skip either, because the write that would record one is
			// the read that just failed.
			name:    "unreadable",
			setup:   func(s *fakeStore) { s.getErr = fmt.Errorf("database is locked") },
			wantErr: "database is locked",
		},
		{
			// A human asked for an automation by id. Its absence is their answer,
			// not something to swallow.
			name:    "deleted under run now",
			manual:  true,
			setup:   func(s *fakeStore) { s.remove("nightly") },
			wantErr: "no automation nightly",
		},
		{
			// Run now works on a disabled row by design: pressing the button is
			// not the schedule.
			name:   "disabled under run now",
			manual: true,
			setup: func(s *fakeStore) {
				disabled := cronAutomation("nightly", "project-a", "*/15 * * * *")
				disabled.Enabled = false
				s.put(disabled)
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeStore(cronAutomation("nightly", "project-a", "*/15 * * * *"))
			testCase.setup(fake)
			started := 0
			scheduler, err := New(Config{
				Store: fake,
				Start: func(store.Automation, string, json.RawMessage) (string, error) {
					started++
					return "item-1", nil
				},
				Clock:  newFakeClock(baseTime),
				Report: func(error) { t.Errorf("nothing reports from attempt itself") },
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			kind := KindCron
			if testCase.manual {
				kind = KindManual
			}
			itemID, err := scheduler.attempt(
				"nightly", Fire{Kind: kind, At: baseTime, ScheduledFor: baseTime}, testCase.manual,
			)

			switch {
			case testCase.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("attempt() error = %v, want one naming %q", err, testCase.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("attempt() error = %v", err)
				}
			}
			// A disabled Run now is the one case that still starts.
			wantStarts := 0
			if testCase.manual && testCase.wantErr == "" {
				wantStarts = 1
			}
			if started != wantStarts {
				t.Fatalf("starts = %d, want %d", started, wantStarts)
			}
			if wantStarts == 0 && itemID != "" {
				t.Fatalf("itemID = %q, want empty", itemID)
			}
			if skips := fake.snapshotSkips(); len(skips) != 0 {
				t.Fatalf("skips = %#v, want none", skips)
			}
		})
	}
}

func TestFailedStartIsRecordedAsASkip(t *testing.T) {
	h := newHarness(t, baseTime, cronAutomation("nightly", "project-a", "*/15 * * * *"))
	h.failStarts(fmt.Errorf("workflow definition %q is invalid", "nightly"))

	h.advance(15 * time.Minute)

	skips := h.store.snapshotSkips()
	if len(skips) != 1 || !strings.HasPrefix(skips[0].reason, "start failed:") {
		t.Fatalf("skips = %#v, want one start-failure skip", skips)
	}
	if !strings.Contains(skips[0].reason, "is invalid") {
		t.Fatalf("skip reason lost the cause: %q", skips[0].reason)
	}
}

func TestOverlapProbeFailureRefusesToStart(t *testing.T) {
	h := newHarness(t, baseTime, cronAutomation("nightly", "project-a", "*/15 * * * *"))
	h.store.mu.Lock()
	h.store.activeErr = fmt.Errorf("database is locked")
	h.store.mu.Unlock()

	h.advance(15 * time.Minute)

	if got := h.startedAutomations(); len(got) != 0 {
		t.Fatalf("starts = %v, want none: not knowing is not permission", got)
	}
	skips := h.store.snapshotSkips()
	if len(skips) != 1 || !strings.HasPrefix(skips[0].reason, "overlap check failed:") {
		t.Fatalf("skips = %#v", skips)
	}
}

func TestBrokenTriggerIsReportedOncePerVersionAndNeverFires(t *testing.T) {
	broken := store.Automation{
		ID: "broken", ProjectID: "project-a", WorkflowID: "nightly", WorkflowScope: "project",
		Name: "Broken", Enabled: true, UpdatedAt: 1,
		Trigger: json.RawMessage(`{"kind":"cron","expr":"not a cron"}`),
	}
	h := newHarness(t, baseTime, broken, cronAutomation("healthy", "project-a", "*/15 * * * *"))

	h.sync()
	h.sync()
	reports := h.snapshotReports()
	if len(reports) != 1 || !strings.Contains(reports[0].Error(), "unusable trigger") {
		t.Fatalf("reports = %v, want exactly one broken-trigger report", reports)
	}

	// The healthy automation next to it keeps firing; a broken row is inert, not
	// contagious, and never records a skip (nothing fired).
	h.advance(15 * time.Minute)
	if got := h.startedAutomations(); !reflect.DeepEqual(got, []string{"healthy"}) {
		t.Fatalf("starts = %v, want [healthy]", got)
	}
	if skips := h.store.snapshotSkips(); len(skips) != 0 {
		t.Fatalf("skips = %#v, want none", skips)
	}

	// Editing the row makes it a new version worth reporting again.
	broken.UpdatedAt = 2
	h.store.put(broken)
	h.sync()
	h.sync()
	if reports := h.snapshotReports(); len(reports) != 2 {
		t.Fatalf("reports after an edit = %v, want two", reports)
	}
}

func TestRunNowBypassesConditionButNotOverlap(t *testing.T) {
	automation := cronAutomation("nightly", "project-a", "0 3 * * *")
	automation.Name = "Nightly audit"
	automation.Enabled = false // Run now is a human's explicit fire, enabled or not.
	automation.Condition = json.RawMessage(`{"eq":{"ref":"trigger.kind","value":"event"}}`)
	h := newHarness(t, baseTime, automation)

	itemID, err := h.scheduler.RunNow("nightly")
	if err != nil {
		t.Fatalf("RunNow() error = %v", err)
	}
	if itemID == "" {
		t.Fatal("RunNow() returned no item id")
	}
	starts := h.snapshotStarts()
	if len(starts) != 1 || starts[0].automationID != "nightly" {
		t.Fatalf("starts = %v", starts)
	}
	if starts[0].goal != "Nightly audit (run now)" {
		t.Fatalf("goal = %q", starts[0].goal)
	}
	trigger, ok := starts[0].seeds[TriggerVariable].(map[string]any)
	if !ok || trigger["kind"] != string(KindManual) {
		t.Fatalf("trigger seed = %#v", starts[0].seeds[TriggerVariable])
	}
	if fires := h.store.snapshotFires(); len(fires) != 1 || fires[0].itemID != itemID {
		t.Fatalf("fire records = %#v", fires)
	}

	// A second press while that run is live is refused loudly, and records
	// nothing: the human is present to read the error.
	h.store.setActive("nightly", store.AutomationRun{ItemID: itemID, State: "running"})
	if _, err := h.scheduler.RunNow("nightly"); err == nil {
		t.Fatal("RunNow() while a run is active returned no error")
	} else if !strings.Contains(err.Error(), itemID) {
		t.Fatalf("RunNow() error = %v, want it to name the active run", err)
	}
	if skips := h.store.snapshotSkips(); len(skips) != 0 {
		t.Fatalf("skips = %#v, want none for a manual refusal", skips)
	}
	if starts := h.snapshotStarts(); len(starts) != 1 {
		t.Fatalf("starts = %v, want the second press to start nothing", starts)
	}
}

func TestRunNowReportsAnUnknownAutomation(t *testing.T) {
	h := newHarness(t, baseTime)
	if _, err := h.scheduler.RunNow("missing"); err == nil {
		t.Fatal("RunNow() on a missing automation returned no error")
	}
}

func TestCommandsAfterStopReportStopped(t *testing.T) {
	h := newHarness(t, baseTime, cronAutomation("nightly", "project-a", "*/15 * * * *"))
	if err := h.scheduler.Stop(contextWithDeadline(t)); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	// Stop is idempotent (the cleanup calls it again) and every later command
	// fails loudly rather than looking like a fire that happened.
	if err := h.scheduler.NotifyItemEvent(ItemEvent{ProjectID: "project-a", State: "done"}); err != ErrStopped {
		t.Fatalf("NotifyItemEvent() after stop = %v, want ErrStopped", err)
	}
	if _, err := h.scheduler.RunNow("nightly"); err != ErrStopped {
		t.Fatalf("RunNow() after stop = %v, want ErrStopped", err)
	}
	if err := h.scheduler.Refresh(); err != ErrStopped {
		t.Fatalf("Refresh() after stop = %v, want ErrStopped", err)
	}
}

func TestListFailureIsReportedAndRetried(t *testing.T) {
	h := newHarness(t, baseTime, cronAutomation("nightly", "project-a", "*/15 * * * *"))
	h.store.mu.Lock()
	h.store.listErr = fmt.Errorf("database is locked")
	h.store.mu.Unlock()
	h.sync()

	if reports := h.snapshotReports(); len(reports) == 0 {
		t.Fatal("a failed automation read was not reported")
	}

	// The retry timer keeps the loop alive: once the read recovers, the schedule
	// comes back without a restart.
	h.store.mu.Lock()
	h.store.listErr = nil
	h.store.mu.Unlock()
	h.advance(refreshRetry)
	h.advance(15 * time.Minute)
	if got := h.startedAutomations(); len(got) == 0 {
		t.Fatal("the schedule did not recover after the read succeeded again")
	}
}
