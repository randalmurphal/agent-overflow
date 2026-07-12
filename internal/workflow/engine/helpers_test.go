package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/profile"
)

type fakeDefinitions struct{ workflows map[string]def.Workflow }

func (f *fakeDefinitions) Resolve(_ context.Context, item store.WorkItem) (def.Workflow, error) {
	workflow, ok := f.workflows[item.WorkflowID]
	if !ok {
		return def.Workflow{}, fmt.Errorf("workflow %q not found", item.WorkflowID)
	}
	return workflow, nil
}

type fakeProfiles struct {
	mu       sync.Mutex
	profiles map[string]*profile.Profile
}

type fakeSpendSource struct {
	mu     sync.Mutex
	spends map[string]Spend
	errs   map[string]error
	calls  []string
}

func (f *fakeSpendSource) ItemSpend(_ context.Context, itemID string) (Spend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, itemID)
	return f.spends[itemID], f.errs[itemID]
}

func (f *fakeSpendSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeProfiles) Profile(_ context.Context, projectID string) (*profile.Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.profiles[projectID]
	if !ok {
		return nil, fmt.Errorf("profile %q not found", projectID)
	}
	copy := *value
	copy.Capacities = make(map[string]int, len(value.Capacities))
	for name, capacity := range value.Capacities {
		copy.Capacities[name] = capacity
	}
	return &copy, nil
}

func (f *fakeProfiles) setCapacity(projectID, name string, capacity int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profiles[projectID].Capacities[name] = capacity
}

type fakeRunner struct {
	mu        sync.Mutex
	callbacks map[string]func(Outcome)
	starts    []RunRequest
	stops     []RunKey
	partials  map[string]json.RawMessage
	startErrs map[string]error
	stopErrs  map[string]error
	startWait map[string]<-chan struct{}
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		callbacks: make(map[string]func(Outcome)), partials: make(map[string]json.RawMessage),
		startErrs: make(map[string]error), stopErrs: make(map[string]error),
		startWait: make(map[string]<-chan struct{}),
	}
}

func runMapKey(key RunKey) string {
	return fmt.Sprintf("%s/%s/%d", key.ItemID, key.PhaseID, key.Attempt)
}

func (f *fakeRunner) Start(ctx context.Context, request RunRequest, entered func(), complete func(Outcome)) error {
	f.mu.Lock()
	f.starts = append(f.starts, request)
	f.callbacks[request.Key.ItemID] = complete
	wait := f.startWait[request.Key.ItemID]
	err := f.startErrs[request.Key.ItemID]
	f.mu.Unlock()
	entered()
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeRunner) Stop(_ context.Context, key RunKey) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, key)
	return append(json.RawMessage(nil), f.partials[runMapKey(key)]...), f.stopErrs[key.ItemID]
}

func (f *fakeRunner) complete(t *testing.T, itemID string, outcome Outcome) {
	t.Helper()
	f.mu.Lock()
	callback := f.callbacks[itemID]
	f.mu.Unlock()
	if callback == nil {
		t.Fatalf("item %q has no active runner callback", itemID)
	}
	callback(outcome)
}

func (f *fakeRunner) started() []RunRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RunRequest(nil), f.starts...)
}

func (f *fakeRunner) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stops)
}

type emittedEvent struct {
	name    string
	payload any
}

type fakeEmitter struct {
	mu     sync.Mutex
	events []emittedEvent
}

func (f *fakeEmitter) Emit(name string, payload any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, emittedEvent{name: name, payload: payload})
}

func (f *fakeEmitter) stateEvents(itemID string) []StateEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []StateEvent
	for _, event := range f.events {
		state, ok := event.payload.(StateEvent)
		if event.name == "workflow:item-state" && ok && state.ItemID == itemID {
			result = append(result, state)
		}
	}
	return result
}

func (f *fakeEmitter) phaseEvents(itemID string) []PhaseEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []PhaseEvent
	for _, event := range f.events {
		phase, ok := event.payload.(PhaseEvent)
		if event.name == "workflow:phase-state" && ok && phase.ItemID == itemID {
			result = append(result, phase)
		}
	}
	return result
}

func (f *fakeEmitter) errorEvents(itemID string) []ErrorEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []ErrorEvent
	for _, event := range f.events {
		value, ok := event.payload.(ErrorEvent)
		if event.name == "workflow:error" && ok && value.ItemID == itemID {
			result = append(result, value)
		}
	}
	return result
}

type testHarness struct {
	store    *store.Store
	engine   *Engine
	runner   *fakeRunner
	emitter  *fakeEmitter
	profiles *fakeProfiles
	spend    *fakeSpendSource
}

func newHarness(t *testing.T, config Config, workflows map[string]def.Workflow, projectIDs []string, beforeStart func(*store.Store)) *testHarness {
	t.Helper()
	database, err := store.New(filepath.Join(t.TempDir(), "engine.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for index, projectID := range projectIDs {
		if err := database.CreateProject(store.Project{
			ID: projectID, Path: filepath.Join(t.TempDir(), projectID), Name: projectID,
			SortPosition: index, CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	if beforeStart != nil {
		beforeStart(database)
	}
	runner := newFakeRunner()
	emitter := &fakeEmitter{}
	profiles := &fakeProfiles{profiles: make(map[string]*profile.Profile)}
	spend := &fakeSpendSource{spends: make(map[string]Spend), errs: make(map[string]error)}
	for _, projectID := range projectIDs {
		profiles.profiles[projectID] = &profile.Profile{Capacities: make(map[string]int)}
	}
	engine, err := New(database, runner, emitter, &fakeDefinitions{workflows: workflows}, profiles, spend, config)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	engine.now = func() time.Time { return time.UnixMilli(100) }
	if err := engine.Start(context.Background()); err != nil {
		database.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
		if err := database.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return &testHarness{store: database, engine: engine, runner: runner, emitter: emitter, profiles: profiles, spend: spend}
}

func testItem(id, projectID, workflowID string, position int) store.WorkItem {
	return store.WorkItem{
		ID: id, ProjectID: projectID, Goal: id, WorkflowID: workflowID,
		WorkflowScope: "shared", State: string(StateQueued), SortPosition: position,
		Seeds: json.RawMessage(`{}`), Source: "manual", CreatedAt: int64(position + 10),
	}
}

func onePhaseWorkflow(id string, resources []string, routes []def.Route) def.Workflow {
	return def.Workflow{ID: id, Phases: []def.Phase{{
		ID: "work", Driver: def.DriverAgent, Resources: resources,
		Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}},
		Gate:    def.Gate{Routes: routes},
	}}}
}

func doneEnvelope(ok bool) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"status":"done","outputs":{"ok":%t},"question":null,"reason":null}`, ok))
}

func questionEnvelope() json.RawMessage {
	return json.RawMessage(`{"status":"question","outputs":null,"question":"Need input","reason":null}`)
}

func stuckEnvelope() json.RawMessage {
	return json.RawMessage(`{"status":"stuck","outputs":null,"question":null,"reason":"Blocked"}`)
}

func requireItemState(t *testing.T, database *store.Store, itemID string, state State, reason Reason) {
	t.Helper()
	item, err := database.GetWorkItem(itemID)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != string(state) || item.Reason != string(reason) {
		t.Fatalf("item %q = state %q reason %q, want %q/%q", itemID, item.State, item.Reason, state, reason)
	}
}
