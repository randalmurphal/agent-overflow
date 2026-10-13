package workflowtools

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/workflow/catalog"
	"agent-overflow/internal/workflow/def"
)

// fakeApp records what each tool asked of the app and answers from fields.
type fakeApp struct {
	mu sync.Mutex
	// calls names every App method in call order.
	calls []string
	// bound is what RunBoundThread answers.
	bound string
	// watches answers successive WatchRun calls; the last repeats.
	watches []Watched
	// watchInputs records every WatchRun request.
	watchInputs []WatchInput
	// statusResting is the resting flag RunStatus's document carries.
	statusResting bool
	control       ControlInput
	resolve       ResolveInput
	err           error
	// fail answers the named method with an error, after the calls before it
	// succeeded.
	fail map[string]error
	// holds records every wake hold, in the order taken.
	holds []*fakeHold
	// armedAt is what ControlRun reports for a scheduled resume.
	armedAt string
}

func (f *fakeApp) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

// errFor is the error the named method answers with.
func (f *fakeApp) errFor(name string) error {
	if err := f.fail[name]; err != nil {
		return err
	}
	return f.err
}

// fakeHold records how a wake hold ended.
type fakeHold struct {
	runID     string
	releases  int
	delivered bool
}

func (h *fakeHold) Release(delivered bool) {
	h.releases++
	h.delivered = delivered
}

func (f *fakeApp) HoldWake(_ context.Context, runID string) WakeHold {
	f.record("HoldWake")
	hold := &fakeHold{runID: runID}
	f.holds = append(f.holds, hold)
	return hold
}

func (f *fakeApp) ListWorkflows(context.Context) (catalog.Listing, error) {
	f.record("ListWorkflows")
	return catalog.Listing{Workflows: []catalog.Entry{}}, f.err
}

func (f *fakeApp) ValidateWorkflow(_ context.Context, id, path string) (def.ValidationResult, error) {
	f.record("ValidateWorkflow:" + id + path)
	return def.ValidationResult{}, f.err
}

func (f *fakeApp) ScaffoldWorkflow(_ context.Context, starter, id string, project bool) (catalog.ScaffoldResult, error) {
	f.record("ScaffoldWorkflow")
	return catalog.ScaffoldResult{}, f.err
}

func (f *fakeApp) StartRun(_ context.Context, input StartInput) (Started, error) {
	f.record("StartRun")
	if err := f.errFor("StartRun"); err != nil {
		return Started{}, err
	}
	started := Started{RunID: "run-1", Result: map[string]any{"itemId": "run-1"}}
	if input.HoldWake {
		hold := &fakeHold{runID: started.RunID}
		f.holds = append(f.holds, hold)
		started.Hold = hold
	}
	return started, nil
}

func (f *fakeApp) RunStatus(_ context.Context, runID string) (any, error) {
	f.record("RunStatus")
	return map[string]any{"itemId": runID, "resting": f.statusResting}, f.errFor("RunStatus")
}

func (f *fakeApp) WatchRun(_ context.Context, input WatchInput) (Watched, error) {
	f.record("WatchRun")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watchInputs = append(f.watchInputs, input)
	if len(f.watches) == 0 {
		return Watched{}, f.errFor("WatchRun")
	}
	next := f.watches[0]
	if len(f.watches) > 1 {
		f.watches = f.watches[1:]
	}
	return next, f.errFor("WatchRun")
}

func (f *fakeApp) RunBoundThread(context.Context, string) (string, error) {
	f.record("RunBoundThread")
	return f.bound, f.errFor("RunBoundThread")
}

func (f *fakeApp) RunOutput(context.Context, string) (any, error) {
	f.record("RunOutput")
	return map[string]any{}, f.err
}

func (f *fakeApp) InspectRun(context.Context, InspectInput) (any, error) {
	f.record("InspectRun")
	return map[string]any{}, f.err
}

func (f *fakeApp) RunNarrative(context.Context, NarrativeInput) (any, error) {
	f.record("RunNarrative")
	return map[string]any{}, f.err
}

func (f *fakeApp) ListRuns(context.Context, bool) (any, error) {
	f.record("ListRuns")
	return []any{}, f.err
}

func (f *fakeApp) ControlRun(_ context.Context, input ControlInput) (ControlDone, error) {
	f.record("ControlRun")
	f.control = input
	return ControlDone{ArmedAt: f.armedAt}, f.errFor("ControlRun")
}

func (f *fakeApp) AmendSeeds(context.Context, string, json.RawMessage) (any, error) {
	f.record("AmendSeeds")
	return map[string]any{}, f.err
}

func (f *fakeApp) GuideRun(context.Context, string, string) (any, error) {
	f.record("GuideRun")
	return map[string]any{}, f.err
}

func (f *fakeApp) ResolveRun(_ context.Context, input ResolveInput) error {
	f.record("ResolveRun")
	f.resolve = input
	return f.errFor("ResolveRun")
}

func (f *fakeApp) AddMemory(context.Context, MemoryInput) (any, error) {
	f.record("AddMemory")
	return map[string]any{}, f.err
}

func (f *fakeApp) ListMemory(context.Context, string, string) (any, error) {
	f.record("ListMemory")
	return map[string]any{}, f.err
}

var interactive = Caller{ThreadID: "thread-1"}

func phase(readOnly bool, grants ...def.Grant) Caller {
	names := make([]string, 0, len(grants))
	for _, grant := range grants {
		names = append(names, string(grant))
	}
	return Caller{ThreadID: "phase-thread", Phase: true, Grants: names, ReadOnly: readOnly}
}

func toolNames(tools []map[string]any) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool["name"].(string))
	}
	return names
}

func publicCode(t *testing.T, err error) (string, string) {
	t.Helper()
	code, message, ok := errorsx.PublicDetails(err)
	if !ok {
		t.Fatalf("error %v is not public", err)
	}
	return code, message
}

// D24's invariant, moved with the grant table: every enforceable grant except
// remote-commands (which the `remote` command and ao-remote-tools carry)
// admits at least one tool, and every tool names only grants the system can
// enforce.
func TestEveryGrantAdmitsATool(t *testing.T) {
	admitted := map[string]bool{}
	for _, spec := range toolTable {
		for _, grant := range spec.grants {
			if !def.KnownGrant(string(grant)) {
				t.Fatalf("%s names unknown grant %q", spec.name, grant)
			}
			admitted[string(grant)] = true
		}
	}
	for _, grant := range def.GrantNames() {
		if grant == string(def.GrantRemoteCommands) {
			continue
		}
		if !admitted[grant] {
			t.Errorf("grant %q admits no workflow tool", grant)
		}
	}
}

func TestToolListFollowsTheCallersGrants(t *testing.T) {
	server := New(&fakeApp{})
	grantFreeReads := []string{"workflow_list", "workflow_validate", "workflow_schema", "memory_list"}
	cases := []struct {
		name   string
		caller Caller
		want   []string
	}{
		{"interactive", interactive, ToolNames()},
		{"phase with no grants", phase(false), []string{"workflow_list", "workflow_validate", "workflow_schema", "workflow_new", "memory_add", "memory_list"}},
		{"read-only phase with no grants", phase(true), grantFreeReads},
		{"read-only phase granted start-run", phase(true, def.GrantStartRun), []string{
			"workflow_list", "workflow_validate", "workflow_schema",
			"run_start", "run_status", "run_watch", "run_output", "run_inspect", "run_narrative",
			"run_control", "run_amend", "run_guide", "memory_list",
		}},
		{"phase granted introspect", phase(true, def.GrantIntrospect), []string{
			"workflow_list", "workflow_validate", "workflow_schema",
			"run_status", "run_watch", "run_output", "run_inspect", "run_narrative", "run_list", "memory_list",
		}},
		{"phase granted resolve", phase(true, def.GrantResolve), append(slices.Clone(grantFreeReads[:3]), "run_resolve", "memory_list")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := toolNames(server.Tools(test.caller)); !slices.Equal(got, test.want) {
				t.Fatalf("tools = %v\nwant    %v", got, test.want)
			}
			for _, name := range ToolNames() {
				if test.caller.Allows(name) != slices.Contains(test.want, name) {
					t.Errorf("Allows(%s) disagrees with the tool list", name)
				}
			}
		})
	}
}

// A call outside the caller's grants is refused by name before the app sees
// it, never narrowed to something else.
func TestCallsOutsideTheGrantsAreRefusedByName(t *testing.T) {
	app := &fakeApp{}
	server := New(app)
	_, err := server.Call(context.Background(), phase(false, def.GrantStartRun), "run_resolve", json.RawMessage(`{"run_id":"r","decision":"answer","answer":"yes"}`))
	code, message := publicCode(t, err)
	if code != CodeGrantRequired || !strings.Contains(message, `"resolve"`) || !strings.Contains(message, "run_resolve") {
		t.Fatalf("refusal = %s: %s", code, message)
	}
	_, err = server.Call(context.Background(), phase(false, def.GrantStartRun), "run_list", nil)
	if code, message := publicCode(t, err); code != CodeGrantRequired || !strings.Contains(message, `"introspect"`) {
		t.Fatalf("refusal = %s: %s", code, message)
	}
	if len(app.calls) != 0 {
		t.Fatalf("a refused call reached the app: %v", app.calls)
	}
}

// The read-only rule: a read-only phase granted start-run can start a run;
// the two tools that write are refused with a reason naming the access level;
// the same phase with write access calls both.
func TestReadOnlyPhasesKeepTheirEnvelopeMemoryChannel(t *testing.T) {
	ctx := context.Background()
	memoryArgs := json.RawMessage(`{"kind":"learning","text":"note"}`)
	newArgs := json.RawMessage(`{"starter":"blank","id":"x","scope":"project"}`)

	app := &fakeApp{}
	server := New(app)
	readOnly := phase(true, def.GrantStartRun)
	if _, err := server.Call(ctx, readOnly, "run_start", json.RawMessage(`{"workflow_id":"flow"}`)); err != nil {
		t.Fatalf("a read-only phase granted start-run could not start a run: %v", err)
	}
	for name, args := range map[string]json.RawMessage{"memory_add": memoryArgs, "workflow_new": newArgs} {
		_, err := server.Call(ctx, readOnly, name, args)
		code, message := publicCode(t, err)
		if code != CodeReadOnly || !strings.Contains(message, "read-only") {
			t.Fatalf("%s from a read-only phase = %s: %s", name, code, message)
		}
	}
	if !slices.Equal(app.calls, []string{"StartRun"}) {
		t.Fatalf("app calls = %v", app.calls)
	}

	writing := phase(false, def.GrantStartRun)
	for name, args := range map[string]json.RawMessage{"memory_add": memoryArgs, "workflow_new": newArgs} {
		if _, err := server.Call(ctx, writing, name, args); err != nil {
			t.Fatalf("%s from a write phase: %v", name, err)
		}
	}
}

// Every tool the guide names is one the caller has, for every caller shape.
func TestInstructionsNameOnlyTheCallersTools(t *testing.T) {
	server := New(&fakeApp{})
	named := regexp.MustCompile("`((?:run|workflow|memory)_[a-z_]+)`")
	for _, caller := range []Caller{
		interactive, phase(false), phase(true), phase(true, def.GrantStartRun),
		phase(true, def.GrantIntrospect), phase(false, def.GrantResolve),
	} {
		guide := server.Instructions(caller)
		for _, match := range named.FindAllStringSubmatch(guide, -1) {
			if !caller.Allows(match[1]) {
				t.Errorf("guide for %+v names %s, which the caller does not have", caller, match[1])
			}
		}
	}
	if guide := server.Instructions(phase(true)); !strings.Contains(guide, "envelope's `memory` field") {
		t.Errorf("a read-only phase's guide does not name its memory channel:\n%s", guide)
	}
}

func TestStatusWithoutAWaitReadsOnce(t *testing.T) {
	app := &fakeApp{statusResting: true}
	result, err := New(app).Call(context.Background(), interactive, "run_status", json.RawMessage(`{"run_id":"run-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	wait := result.(Wait)
	if wait.Outcome != OutcomeResting || wait.Notify != nil || !slices.Equal(app.calls, []string{"RunStatus"}) {
		t.Fatalf("wait = %+v, calls = %v", wait, app.calls)
	}
}

func TestWaitReturnsWhenTheRunRests(t *testing.T) {
	app := &fakeApp{watches: []Watched{{Cursor: 4}, {Cursor: 6, Transitions: 1}, {Cursor: 7, Transitions: 1, Resting: true}}}
	result, err := New(app).Call(context.Background(), interactive, "run_status", json.RawMessage(`{"run_id":"run-1","wait_seconds":600}`))
	if err != nil {
		t.Fatal(err)
	}
	wait := result.(Wait)
	if wait.Outcome != OutcomeResting || wait.Notify != nil {
		t.Fatalf("wait = %+v", wait)
	}
	// Each hold continues from the cursor the previous one returned.
	if len(app.watchInputs) != 3 || app.watchInputs[0].Cursor != 0 || app.watchInputs[1].Cursor != 4 || app.watchInputs[2].Cursor != 6 {
		t.Fatalf("watch inputs = %+v", app.watchInputs)
	}
}

// A wait that ends first is backgrounded and always says whether the rest
// will reach the caller, in each of the four cases.
func TestBackgroundedWaitStatesNotify(t *testing.T) {
	cases := []struct {
		name   string
		caller Caller
		bound  string
		armed  bool
		detail string
	}{
		{"bound to the caller", interactive, interactive.ThreadID, true, notifyBound},
		{"unbound", interactive, "", false, notifyUnbound},
		{"bound elsewhere", interactive, "other-thread", false, "This run reports to thread other-thread, not to yours; poll run_status again."},
		{"phase caller", phase(true, def.GrantStartRun), "", false, notifyPhase},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			app := &fakeApp{bound: test.bound, watches: []Watched{{Cursor: 1}}}
			server := New(app)
			// Every hold spends the whole wait, so the first one ends it.
			server.now = func() time.Time { now = now.Add(31 * time.Second); return now }
			result, err := server.Call(context.Background(), test.caller, "run_status", json.RawMessage(`{"run_id":"run-1","wait_seconds":30}`))
			if err != nil {
				t.Fatal(err)
			}
			wait := result.(Wait)
			if wait.Outcome != OutcomeBackgrounded || wait.Notify == nil {
				t.Fatalf("wait = %+v", wait)
			}
			if wait.Notify.Armed != test.armed || wait.Notify.Detail != test.detail {
				t.Fatalf("notify = %+v, want armed=%t %q", wait.Notify, test.armed, test.detail)
			}
		})
	}
}

// notify with no wait answers for a run that is still going, and not for one
// that is already resting.
func TestNotifyWithoutAWait(t *testing.T) {
	app := &fakeApp{bound: interactive.ThreadID}
	result, err := New(app).Call(context.Background(), interactive, "run_status", json.RawMessage(`{"run_id":"run-1","notify":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if wait := result.(Wait); wait.Outcome != OutcomeRunning || wait.Notify == nil || !wait.Notify.Armed {
		t.Fatalf("wait = %+v", wait)
	}
	app = &fakeApp{statusResting: true}
	result, err = New(app).Call(context.Background(), interactive, "run_status", json.RawMessage(`{"run_id":"run-1","notify":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if wait := result.(Wait); wait.Outcome != OutcomeResting || wait.Notify != nil {
		t.Fatalf("wait = %+v", wait)
	}
}

func TestStartWithoutAWaitReturnsTheStartDocument(t *testing.T) {
	app := &fakeApp{}
	result, err := New(app).Call(context.Background(), interactive, "run_start", json.RawMessage(`{"workflow_id":"flow","seeds":{"goal":"x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if string(encoded) != `{"start":{"itemId":"run-1"}}` || !slices.Equal(app.calls, []string{"StartRun"}) {
		t.Fatalf("result = %s, calls = %v", encoded, app.calls)
	}
	_, err = New(app).Call(context.Background(), interactive, "run_start", json.RawMessage(`{"workflow_id":"flow","seeds":[1]}`))
	if code, _ := publicCode(t, err); code != CodeInvalidRequest {
		t.Fatalf("array seeds = %s", code)
	}
}

func TestWaitsAreBounded(t *testing.T) {
	server := New(&fakeApp{})
	// A clock that jumps past any deadline, so a wait wrongly admitted ends at
	// once and fails below instead of holding the test for fifteen minutes.
	now := time.Unix(1_700_000_000, 0)
	server.now = func() time.Time { now = now.Add(time.Hour); return now }
	for tool, args := range map[string]string{
		"run_status": `{"run_id":"r","wait_seconds":901}`,
		"run_watch":  `{"run_id":"r","wait_seconds":901}`,
		"run_start":  `{"workflow_id":"w","wait_seconds":901}`,
	} {
		_, err := server.Call(context.Background(), interactive, tool, json.RawMessage(args))
		if code, _ := publicCode(t, err); code != CodeInvalidRequest {
			t.Errorf("%s accepted a wait past %d: %v", tool, MaxWaitSeconds, err)
		}
	}
	// The refusal comes before the start: a run started and then refused
	// would be one the caller never hears of.
	app := &fakeApp{}
	_, err := New(app).Call(context.Background(), interactive, "run_start", json.RawMessage(`{"workflow_id":"w","wait_seconds":901}`))
	if err == nil || len(app.calls) != 0 {
		t.Fatalf("an over-limit wait reached the app: err=%v calls=%v", err, app.calls)
	}
}

// Once StartRun commits, run_start answers with the run whatever fails after:
// a failed read reported as a failed start would have the caller start it
// again, and an interactive start has no surface-and-skip to stop that.
func TestStartKeepsTheRunWhenReadingItBackFails(t *testing.T) {
	broken := errors.New("store unavailable")
	for _, method := range []string{"WatchRun", "RunStatus"} {
		t.Run(method, func(t *testing.T) {
			app := &fakeApp{fail: map[string]error{method: broken}, watches: []Watched{{Cursor: 1, Resting: true}}}
			result, err := New(app).Call(context.Background(), interactive, "run_start", json.RawMessage(`{"workflow_id":"flow","wait_seconds":60}`))
			if err != nil {
				t.Fatalf("a %s failure after the start failed the call: %v", method, err)
			}
			started := result.(StartResult)
			if started.Start.(map[string]any)["itemId"] != "run-1" || started.Outcome != OutcomeUnknown || started.Run != nil {
				t.Fatalf("result = %+v", started)
			}
			if !strings.Contains(started.ReadError, "store unavailable") {
				t.Fatalf("readError = %q", started.ReadError)
			}
			if len(app.holds) != 1 || app.holds[0].releases != 1 || app.holds[0].delivered {
				t.Fatalf("hold = %+v, want released once, not delivered", app.holds)
			}
		})
	}
}

// The binding lookup comes after the status read: its failure costs the
// caller the notify answer, never the run it started or the status it read.
func TestNotifyLookupFailureKeepsTheRunAndItsStatus(t *testing.T) {
	broken := errors.New("binding unreadable")
	now := time.Unix(1000, 0)
	app := &fakeApp{fail: map[string]error{"RunBoundThread": broken}, watches: []Watched{{Cursor: 1}}}
	server := New(app)
	server.now = func() time.Time { now = now.Add(31 * time.Second); return now }
	result, err := server.Call(context.Background(), interactive, "run_start", json.RawMessage(`{"workflow_id":"flow","wait_seconds":30}`))
	if err != nil {
		t.Fatalf("a notify failure after the start failed the call: %v", err)
	}
	started := result.(StartResult)
	if started.Start == nil || started.Outcome != OutcomeBackgrounded || started.Run == nil {
		t.Fatalf("result = %+v", started)
	}
	if started.Notify == nil || started.Notify.Armed || !strings.Contains(started.Notify.Detail, "binding unreadable") {
		t.Fatalf("notify = %+v", started.Notify)
	}

	app = &fakeApp{fail: map[string]error{"RunBoundThread": broken}, watches: []Watched{{Cursor: 1}}}
	server = New(app)
	server.now = func() time.Time { now = now.Add(31 * time.Second); return now }
	status, err := server.Call(context.Background(), interactive, "run_status", json.RawMessage(`{"run_id":"run-1","wait_seconds":30}`))
	if err != nil {
		t.Fatalf("a notify failure discarded the status: %v", err)
	}
	if wait := status.(Wait); wait.Run == nil || wait.Notify == nil || !strings.Contains(wait.Notify.Detail, "binding unreadable") {
		t.Fatalf("status = %+v", wait)
	}
}

// An action that took effect is reported as done when reading the run back
// fails, so the caller does not act twice.
func TestActionsReportDoneWhenReadingTheRunBackFails(t *testing.T) {
	broken := errors.New("store unavailable")
	cases := []struct {
		tool, args, method, action, armedAt string
	}{
		{"run_control", `{"run_id":"run-1","action":"pause"}`, "ControlRun", "pause", ""},
		{"run_control", `{"run_id":"run-1","action":"resume","at":"+1h"}`, "ControlRun", "resume", "2030-01-01T00:00:00Z"},
		{"run_resolve", `{"run_id":"run-1","decision":"approve"}`, "ResolveRun", "approve", ""},
	}
	for _, test := range cases {
		t.Run(test.tool+" "+test.action, func(t *testing.T) {
			app := &fakeApp{fail: map[string]error{"RunStatus": broken}, armedAt: test.armedAt}
			result, err := New(app).Call(context.Background(), interactive, test.tool, json.RawMessage(test.args))
			if err != nil {
				t.Fatalf("a read failure after %s failed the call: %v", test.method, err)
			}
			done := result.(ActionDone)
			if !done.Done || done.RunID != "run-1" || done.Action != test.action || done.ArmedAt != test.armedAt || !strings.Contains(done.ReadError, "store unavailable") {
				t.Fatalf("result = %+v", done)
			}
			if !slices.Equal(app.calls, []string{test.method, "RunStatus"}) {
				t.Fatalf("calls = %v", app.calls)
			}
		})
	}
	// An action the app refused is still a failure.
	app := &fakeApp{fail: map[string]error{"ControlRun": broken}}
	if _, err := New(app).Call(context.Background(), interactive, "run_control", json.RawMessage(`{"run_id":"run-1","action":"pause"}`)); err == nil {
		t.Fatal("a refused action reported success")
	}
}

// A call that waits holds the run's wake to its thread and says, when it ends,
// whether it returned the rest; a call that does not wait, or a phase caller
// that is never woken, holds nothing.
func TestWaitingCallsHoldTheWake(t *testing.T) {
	ctx := context.Background()
	t.Run("start that returns the rest", func(t *testing.T) {
		app := &fakeApp{watches: []Watched{{Cursor: 1, Resting: true}}}
		if _, err := New(app).Call(ctx, interactive, "run_start", json.RawMessage(`{"workflow_id":"flow","wait_seconds":60}`)); err != nil {
			t.Fatal(err)
		}
		if len(app.holds) != 1 || app.holds[0].releases != 1 || !app.holds[0].delivered {
			t.Fatalf("holds = %+v, want one released as delivered", app.holds)
		}
	})
	t.Run("start that backgrounds", func(t *testing.T) {
		now := time.Unix(1000, 0)
		app := &fakeApp{watches: []Watched{{Cursor: 1}}}
		server := New(app)
		server.now = func() time.Time { now = now.Add(31 * time.Second); return now }
		if _, err := server.Call(ctx, interactive, "run_start", json.RawMessage(`{"workflow_id":"flow","wait_seconds":30}`)); err != nil {
			t.Fatal(err)
		}
		if len(app.holds) != 1 || app.holds[0].releases != 1 || app.holds[0].delivered {
			t.Fatalf("holds = %+v, want one released as not delivered", app.holds)
		}
	})
	t.Run("status that returns the rest", func(t *testing.T) {
		app := &fakeApp{watches: []Watched{{Cursor: 1, Resting: true}}}
		if _, err := New(app).Call(ctx, interactive, "run_status", json.RawMessage(`{"run_id":"run-1","wait_seconds":60}`)); err != nil {
			t.Fatal(err)
		}
		if len(app.holds) != 1 || app.holds[0].runID != "run-1" || app.holds[0].releases != 1 || !app.holds[0].delivered {
			t.Fatalf("holds = %+v", app.holds)
		}
		if app.calls[0] != "HoldWake" {
			t.Fatalf("the hold was taken after the wait began: %v", app.calls)
		}
	})
	t.Run("status whose wait fails", func(t *testing.T) {
		app := &fakeApp{fail: map[string]error{"WatchRun": errors.New("gone")}}
		if _, err := New(app).Call(ctx, interactive, "run_status", json.RawMessage(`{"run_id":"run-1","wait_seconds":60}`)); err == nil {
			t.Fatal("a failed wait answered")
		}
		if len(app.holds) != 1 || app.holds[0].releases != 1 || app.holds[0].delivered {
			t.Fatalf("holds = %+v", app.holds)
		}
	})
	for name, test := range map[string]struct {
		caller Caller
		tool   string
		args   string
	}{
		"start without a wait":   {interactive, "run_start", `{"workflow_id":"flow"}`},
		"status without a wait":  {interactive, "run_status", `{"run_id":"run-1","notify":true}`},
		"phase that waits":       {phase(false, def.GrantStartRun), "run_status", `{"run_id":"run-1","wait_seconds":60}`},
		"phase start that waits": {phase(false, def.GrantStartRun), "run_start", `{"workflow_id":"flow","wait_seconds":60}`},
	} {
		t.Run(name, func(t *testing.T) {
			app := &fakeApp{watches: []Watched{{Cursor: 1, Resting: true}}}
			if _, err := New(app).Call(ctx, test.caller, test.tool, json.RawMessage(test.args)); err != nil {
				t.Fatal(err)
			}
			if len(app.holds) != 0 {
				t.Fatalf("holds = %+v, want none", app.holds)
			}
		})
	}
}

func TestWatchReturnsOnTheFirstTransition(t *testing.T) {
	app := &fakeApp{watches: []Watched{{Cursor: 3}, {Cursor: 3}, {Cursor: 5, Transitions: 2, Result: "moved"}}}
	result, err := New(app).Call(context.Background(), interactive, "run_watch", json.RawMessage(`{"run_id":"r","cursor":3,"wait_seconds":600}`))
	if err != nil || result != "moved" {
		t.Fatalf("result = %v, err = %v", result, err)
	}
	app = &fakeApp{watches: []Watched{{Cursor: 9, Result: "now"}}}
	result, err = New(app).Call(context.Background(), interactive, "run_watch", json.RawMessage(`{"run_id":"r"}`))
	if err != nil || result != "now" || len(app.watchInputs) != 1 {
		t.Fatalf("a first watch did not answer at once: %v, %v, %+v", result, err, app.watchInputs)
	}
}

func TestControlTakesOnlyItsActionsFields(t *testing.T) {
	server := New(&fakeApp{})
	for _, args := range []string{
		`{"run_id":"r","action":"pause","note":"x"}`,
		`{"run_id":"r","action":"cancel","phase_id":"p"}`,
		`{"run_id":"r","action":"resume","at":"+1h","phase_id":"p"}`,
		`{"run_id":"r","action":"retry_unit"}`,
		`{"run_id":"r","action":"rerun","clear":true}`,
		`{"run_id":"r","action":"explode"}`,
	} {
		if _, err := server.Call(context.Background(), interactive, "run_control", json.RawMessage(args)); err == nil {
			t.Errorf("%s was accepted", args)
		} else if code, _ := publicCode(t, err); code != CodeInvalidRequest {
			t.Errorf("%s refused as %s", args, code)
		}
	}
	app := &fakeApp{}
	if _, err := New(app).Call(context.Background(), interactive, "run_control", json.RawMessage(`{"run_id":"r","action":"resume","phase_id":"p","refresh_def":true}`)); err != nil {
		t.Fatal(err)
	}
	if app.control.Action != "resume" || app.control.PhaseID != "p" || !app.control.RefreshDef {
		t.Fatalf("control = %+v", app.control)
	}
}

func TestResolveDecisions(t *testing.T) {
	server := New(&fakeApp{})
	for _, args := range []string{
		`{"run_id":"r","decision":"answer"}`,
		`{"run_id":"r","decision":"approve","answer":"x"}`,
		`{"run_id":"r","decision":"answer","answer":"x","note":"y"}`,
		`{"run_id":"r","decision":"maybe"}`,
	} {
		if _, err := server.Call(context.Background(), interactive, "run_resolve", json.RawMessage(args)); err == nil {
			t.Errorf("%s was accepted", args)
		}
	}
}

func TestArgumentsAreClosedAndValidated(t *testing.T) {
	server := New(&fakeApp{})
	for tool, args := range map[string]string{
		"workflow_validate": `{}`,
		"workflow_new":      `{"starter":"blank","id":"x","scope":"elsewhere"}`,
		"run_status":        `{"run_id":"r","surprise":true}`,
		"run_narrative":     `{"run_id":"r"}`,
		"run_inspect":       `{"run_id":"r","attempt":2}`,
		"memory_add":        `{"kind":"learning"}`,
		"run_amend":         `{"run_id":"r"}`,
	} {
		if _, err := server.Call(context.Background(), interactive, tool, json.RawMessage(args)); err == nil {
			t.Errorf("%s %s was accepted", tool, args)
		}
	}
	if _, err := server.Call(context.Background(), interactive, "workflow_validate", json.RawMessage(`{"path":"relative.yaml"}`)); err == nil {
		t.Error("a relative path was accepted")
	}
	if _, err := server.Call(context.Background(), interactive, "nope", nil); err == nil {
		t.Error("an unknown tool was accepted")
	}
	if _, err := server.Call(context.Background(), Caller{}, "workflow_list", nil); err == nil {
		t.Error("a call with no calling thread was accepted")
	}
}

// An app refusal reaches the model as a public error carrying the app's prose.
func TestAppRefusalsAreReported(t *testing.T) {
	app := &fakeApp{err: errorsx.Public("x", "run r belongs to another project", nil)}
	_, err := New(app).Call(context.Background(), interactive, "run_output", json.RawMessage(`{"run_id":"r"}`))
	if _, message := publicCode(t, err); message != "run r belongs to another project" {
		t.Fatalf("message = %q", message)
	}
	app = &fakeApp{err: context.DeadlineExceeded}
	_, err = New(app).Call(context.Background(), interactive, "run_list", nil)
	if code, _ := publicCode(t, err); code != CodeFailed {
		t.Fatalf("code = %q", code)
	}
}

func TestSchemaReturnsTheAuthoringSchema(t *testing.T) {
	result, err := New(&fakeApp{}).Call(context.Background(), interactive, "workflow_schema", nil)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(result.(json.RawMessage), &schema); err != nil || schema["$schema"] == nil {
		t.Fatalf("schema = %v, err = %v", schema, err)
	}
}
