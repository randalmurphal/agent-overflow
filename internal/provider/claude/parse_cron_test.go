package claude

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

const (
	cronCaptureFixture = "../../../docs/references/fixtures/claude/cron_create_fire_20261010.ndjson"
	cronLoopFixture    = "../../../docs/references/fixtures/claude/cron_loop_midturn_20261010.ndjson"
	cronCapturePrompt  = "Reply with exactly the text CRON-FIRED and call no tools."
)

func scheduledJobSnapshots(t *testing.T, events []provider.ProviderEvent) [][]string {
	t.Helper()
	var out [][]string
	for _, evt := range events {
		if evt.Kind != provider.EventScheduledJobs {
			continue
		}
		var meta provider.ScheduledJobsMeta
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			t.Fatalf("decode scheduled jobs: %v", err)
		}
		ids := []string{}
		for _, job := range meta.Jobs {
			if job.FireByUnixMs <= 0 || job.Cron == "" {
				t.Fatalf("job %+v carries no deadline or cron", job)
			}
			ids = append(ids, job.ID)
		}
		out = append(out, ids)
	}
	return out
}

func scheduledPrompts(t *testing.T, events []provider.ProviderEvent) []provider.ProviderEvent {
	t.Helper()
	var out []provider.ProviderEvent
	for _, evt := range events {
		if evt.Kind != provider.EventUserText {
			continue
		}
		var meta map[string]any
		_ = json.Unmarshal(evt.Meta, &meta)
		if meta["origin"] == ScheduledTurnOrigin {
			out = append(out, evt)
		}
	}
	return out
}

// The 2.1.284 capture drives every ack shape: CronCreate of a recurring
// job, three fires, then CronList, a one-shot CronCreate, a CronDelete of
// an unknown id (refused), two real CronDeletes and an empty CronList.
// The job table follows the CLI's, and every fire's stored prompt becomes
// a scheduled user row under the fire's own command uuid.
func TestReplayCronCaptureTracksTheArmedJobs(t *testing.T) {
	events := replayFixture(t, cronCaptureFixture)

	got := scheduledJobSnapshots(t, events)
	want := [][]string{{"6ae146b0"}, {"6ae146b0", "718ab791"}, {"718ab791"}, {}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("job snapshots = %v, want %v", got, want)
	}

	prompts := scheduledPrompts(t, events)
	wantIDs := []string{
		"e5efdb27-b118-4745-914d-2dafe83dd1fa",
		"35ce2662-cc72-46e7-923f-9b51f5b698aa",
		"879bd915-5480-4e99-953e-9ef1ed4b6345",
	}
	if len(prompts) != len(wantIDs) {
		t.Fatalf("scheduled prompt rows = %d, want %d", len(prompts), len(wantIDs))
	}
	for i, evt := range prompts {
		var meta map[string]any
		_ = json.Unmarshal(evt.Meta, &meta)
		if evt.Content != cronCapturePrompt || meta["provider_item_id"] != wantIDs[i] {
			t.Fatalf("fire %d row = %q %v, want %q under %s", i, evt.Content, meta, cronCapturePrompt, wantIDs[i])
		}
	}
	// An AO send's prompt is never a scheduled row.
	for _, evt := range events {
		if evt.Kind == provider.EventUserText && strings.Contains(evt.Content, "Call the CronCreate tool") {
			var meta map[string]any
			_ = json.Unmarshal(evt.Meta, &meta)
			if meta["origin"] != nil {
				t.Fatalf("AO's own send carries origin %v", meta["origin"])
			}
		}
	}
}

// /loop arms through the same CronCreate, and a fire the CLI held
// through a busy turn arrives after that turn as its own scheduled row.
func TestReplayLoopCaptureReadsTheHeldFire(t *testing.T) {
	events := replayFixture(t, cronLoopFixture)
	if got := scheduledJobSnapshots(t, events); len(got) == 0 || !slices.Equal(got[0], []string{"b919c533"}) {
		t.Fatalf("job snapshots = %v, want b919c533 armed first", got)
	}
	prompts := scheduledPrompts(t, events)
	if len(prompts) != 1 || prompts[0].Content != "Reply with exactly the text LOOP-PING and call no tools." {
		t.Fatalf("scheduled rows = %+v, want the one held fire", prompts)
	}
}

// A refused CronDelete changed nothing in the CLI, and an ack whose shape
// is not the one documented must not release an armed job.
func TestCronAcksThatProveNothingLeaveTheTableAlone(t *testing.T) {
	p := NewParser()
	if changed, problem := p.armCronJob("6ae146b0", "* * * * *", true, time.Now()); !changed || problem != "" {
		t.Fatalf("arm: changed=%v problem=%q", changed, problem)
	}
	for name, tc := range map[string]struct {
		tool   string
		block  string
		result string
	}{
		"refused delete":       {cronDeleteToolName, `{"is_error":true}`, `"Error: No scheduled job with id '6ae146b0'"`},
		"delete without id":    {cronDeleteToolName, `{}`, `{"deleted":true}`},
		"list without jobs":    {cronListToolName, `{}`, `{"items":[]}`},
		"create without flags": {cronCreateToolName, `{}`, `{"id":"aaaa0000"}`},
		// A success-shaped ack whose is_error does not decode is a refusal.
		"undecodable is_error on delete": {cronDeleteToolName, `{"is_error":"false"}`, `{"id":"6ae146b0"}`},
		"undecodable is_error on create": {cronCreateToolName, `{"is_error":0}`, `{"id":"aaaa0000","recurring":true}`},
	} {
		var block map[string]json.RawMessage
		_ = json.Unmarshal([]byte(tc.block), &block)
		events := p.appendCronAckEvents(nil, testThread, time.Now(), "toolu_x", cronToolUse{name: tc.tool, cron: "* * * * *"}, block, json.RawMessage(tc.result))
		if len(events) != 0 {
			t.Fatalf("%s emitted %+v", name, events)
		}
		if _, ok := p.cronJobs["6ae146b0"]; !ok || len(p.cronJobs) != 1 {
			t.Fatalf("%s changed the table: %v", name, p.cronJobs)
		}
	}
}

// A job the CLI accepted with an expression AO cannot read is reported,
// not silently dropped: the reaper cannot protect it.
func TestCronCreateWithAnUnreadableExpressionIsReported(t *testing.T) {
	p := NewParser()
	events := p.appendCronAckEvents(nil, testThread, time.Now(), "toolu_x",
		cronToolUse{name: cronCreateToolName, cron: "@hourly"}, nil, json.RawMessage(`{"id":"aaaa0000","recurring":true}`))
	if len(events) != 1 || events[0].Kind != provider.EventError || !strings.Contains(events[0].Content, "@hourly") {
		t.Fatalf("events = %+v, want one error naming the expression", events)
	}
}

// A fire re-anchors every recurring job whose match has come, which only
// ever moves its deadline later, and releases a due one-shot. Jobs not
// yet due are untouched, and a fire before any match changes nothing.
func TestScheduledFireMovesDueJobsOnly(t *testing.T) {
	created := time.Date(2026, 10, 7, 10, 0, 30, 0, time.UTC)
	p := NewParser()
	p.armCronJob("every0001", "* * * * *", true, created)
	p.armCronJob("hourly002", "0 * * * *", true, created)
	p.armCronJob("oneshot03", "1 10 * * *", false, created)
	p.armCronJob("oneshot04", "0 12 * * *", false, created)
	before := p.cronJobs["every0001"].fireBy

	if events := p.noteScheduledFire(nil, testThread, created.Add(20*time.Second)); len(events) != 0 {
		t.Fatalf("a fire before any match changed the table: %+v", events)
	}

	fired := time.Date(2026, 10, 7, 10, 1, 13, 0, time.UTC)
	events := p.noteScheduledFire(nil, testThread, fired)
	if got := scheduledJobSnapshots(t, events); len(got) != 1 || !slices.Equal(got[0], []string{"every0001", "hourly002", "oneshot04"}) {
		t.Fatalf("snapshot after fire = %v", got)
	}
	every := p.cronJobs["every0001"]
	if !every.fireBy.After(before) || !every.match.Equal(time.Date(2026, 10, 7, 10, 2, 0, 0, time.UTC)) {
		t.Fatalf("recurring job not re-anchored: match %v fireBy %v (was %v)", every.match, every.fireBy, before)
	}
	if hourly := p.cronJobs["hourly002"]; !hourly.match.Equal(time.Date(2026, 10, 7, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("a job not yet due moved: %v", hourly.match)
	}
}

// The table is bounded above the CLI's own 50-job limit.
func TestCronJobTableIsBounded(t *testing.T) {
	p := NewParser()
	for i := range maxTrackedCronJobs + 5 {
		p.armCronJob(strings.Repeat("0", 4)+string(rune('a'+i%26))+string(rune('a'+i/26)), "* * * * *", true, time.Now())
	}
	if len(p.cronJobs) != maxTrackedCronJobs {
		t.Fatalf("table holds %d jobs, want the cap %d", len(p.cronJobs), maxTrackedCronJobs)
	}
	if maxTrackedCronJobs < 50 {
		t.Fatalf("cap %d is below the CLI's own limit of 50", maxTrackedCronJobs)
	}
}
