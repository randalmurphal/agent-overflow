package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/profile"
)

type fakeDefinitions struct {
	mu        sync.Mutex
	workflows map[string]def.Workflow
	// callResolves records every call-time resolution, so a test can assert that
	// a child was resolved fresh per invocation rather than reusing the parent's
	// frozen snapshot.
	callResolves []string
}

func (f *fakeDefinitions) Resolve(_ context.Context, item store.WorkItem) (ResolvedDefinition, error) {
	return f.resolve(item.WorkflowID)
}

func (f *fakeDefinitions) ResolveCall(_ context.Context, _ string, workflowID string) (ResolvedDefinition, error) {
	f.mu.Lock()
	f.callResolves = append(f.callResolves, workflowID)
	f.mu.Unlock()
	return f.resolve(workflowID)
}

func (f *fakeDefinitions) resolve(workflowID string) (ResolvedDefinition, error) {
	// Held across the whole resolution because a test edits the set while the
	// engine's goroutine may be resolving from it, which is the point: a
	// definition on disk changes under a parked run.
	f.mu.Lock()
	defer f.mu.Unlock()
	workflow, ok := f.workflows[workflowID]
	if !ok {
		return ResolvedDefinition{}, fmt.Errorf("workflow %q not found", workflowID)
	}
	// Mirror the app source: the frozen need accounts for the whole call graph,
	// so a caller of a writing workflow provisions a worktree.
	need, err := def.PropagatedWorkspaceNeed(workflow, fakeCallResolver{workflows: f.workflows})
	if err != nil {
		return ResolvedDefinition{}, err
	}
	return ResolvedDefinition{Workflow: workflow, Scope: def.ScopeShared, WorkspaceNeed: need}, nil
}

// edit replaces what the source answers for one workflow id, which is a
// definition file changing on disk while a run that froze the old one is
// parked.
func (f *fakeDefinitions) edit(workflowID string, workflow def.Workflow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workflows[workflowID] = workflow
}

func (f *fakeDefinitions) callResolveCount(workflowID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, resolved := range f.callResolves {
		if resolved == workflowID {
			count++
		}
	}
	return count
}

// fakeCallResolver is the def-side view of the same definition set. It is a
// separate type because def.CallResolver and engine.DefinitionSource both name
// a method `ResolveCall` with different signatures.
type fakeCallResolver struct{ workflows map[string]def.Workflow }

func (f fakeCallResolver) ResolveCall(id string) (def.ResolvedWorkflow, error) {
	workflow, ok := f.workflows[id]
	if !ok {
		return def.ResolvedWorkflow{}, fmt.Errorf("workflow %q not found", id)
	}
	return def.ResolvedWorkflow{Workflow: workflow, Scope: def.ScopeShared}, nil
}

type fakeProfiles struct {
	mu       sync.Mutex
	profiles map[string]*profile.Profile
}

// fakeSpendSource prices per item and aggregates over the run tree, the way the
// app's store-backed source does. Tests set one item's spend — including a
// called run's — and assert what the root's budget check sees.
type fakeSpendSource struct {
	mu     sync.Mutex
	store  *store.Store
	spends map[string]Spend
	errs   map[string]error
	calls  []string
}

func (f *fakeSpendSource) TreeSpend(_ context.Context, rootItemID string) (Spend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rootItemID)
	if err := f.errs[rootItemID]; err != nil {
		return Spend{}, err
	}
	return f.treeSpend(rootItemID)
}

func (f *fakeSpendSource) treeSpend(itemID string) (Spend, error) {
	total := f.spends[itemID]
	if f.store == nil {
		return total, nil
	}
	children, err := f.store.ListWorkItemChildren(itemID)
	if err != nil {
		return Spend{}, err
	}
	for _, child := range children {
		childSpend, err := f.treeSpend(child.ID)
		if err != nil {
			return Spend{}, err
		}
		total.Tokens += childSpend.Tokens
		total.USD += childSpend.USD
		// An estimate anywhere in the tree makes the tree's dollars an estimate,
		// and unpriced rows accumulate — the app's source composes both the same
		// way, and a fake that dropped them would let a test pass on a number the
		// real one would have flagged.
		total.Estimated = total.Estimated || childSpend.Estimated
		total.Unpriced += childSpend.Unpriced
	}
	return total, nil
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

func (f *fakeProfiles) setMaxFanOutWidth(projectID string, width int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profiles[projectID].MaxFanOutWidth = &width
}

// remove makes the source fail for one project, which is what a profile that
// cannot be read looks like to the engine.
func (f *fakeProfiles) remove(projectID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.profiles, projectID)
}

// fakeRunner records every start and lets a test complete one by key. The
// per-run maps accept either a whole run key (`item/phase/attempt[/unit]`) or a
// bare item id, so a phase-level test can stay item-scoped while a fan-out test
// addresses one unit precisely.
type fakeRunner struct {
	mu           sync.Mutex
	callbacks    map[string]func(Outcome)
	lastItemRun  map[string]RunKey
	starts       []RunRequest
	stops        []RunKey
	takeoverKeys []RunKey
	partials     map[string]json.RawMessage
	startErrs    map[string]error
	startError   func(RunRequest) error
	stopErrs     map[string]error
	startWait    map[string]<-chan struct{}
	// ack stands in for the app runner's send door: a successful start hands the
	// prompt to a live provider session and reports the dispatch, which is what
	// settles the attempt's owed feedback. The engine no longer infers it from the
	// start's success — a start can return nil having DROPPED its send.
	ack func(RunKey) error
	// dropSend names the keys whose start succeeds but whose send never reached a
	// model. Nothing acks for those, which is the case C2 exists for.
	dropSend map[string]bool
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		callbacks: make(map[string]func(Outcome)), lastItemRun: make(map[string]RunKey),
		partials:  make(map[string]json.RawMessage),
		startErrs: make(map[string]error), stopErrs: make(map[string]error),
		startWait: make(map[string]<-chan struct{}),
		dropSend:  make(map[string]bool),
	}
}

func runMapKey(key RunKey) string {
	if key.UnitID != "" {
		return fmt.Sprintf("%s/%s/%d/%s", key.ItemID, key.PhaseID, key.Attempt, key.UnitID)
	}
	return fmt.Sprintf("%s/%s/%d", key.ItemID, key.PhaseID, key.Attempt)
}

func lookupByRun[T any](values map[string]T, key RunKey) T {
	if value, ok := values[runMapKey(key)]; ok {
		return value
	}
	return values[key.ItemID]
}

func (f *fakeRunner) Start(ctx context.Context, request RunRequest, entered func(), complete func(Outcome)) error {
	f.mu.Lock()
	f.starts = append(f.starts, request)
	f.callbacks[runMapKey(request.Key)] = complete
	if request.Key.UnitID == "" {
		f.lastItemRun[request.Key.ItemID] = request.Key
	}
	wait := lookupByRun(f.startWait, request.Key)
	err := lookupByRun(f.startErrs, request.Key)
	if f.startError != nil {
		if dynamic := f.startError(request); dynamic != nil {
			err = dynamic
		}
	}
	ack, dropped := f.ack, f.dropSend[runMapKey(request.Key)]
	f.mu.Unlock()
	entered()
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err == nil && !dropped && ack != nil && sendsPrompt(request) {
		// The real send door acks from INSIDE the start, before it returns: the
		// opening send is what a start is for. Acking here keeps that ordering.
		_ = ack(request.Key)
	}
	return err
}

// sendsPrompt mirrors where the real runner's send door actually acks: only an
// agent element's start dispatches a prompt to a provider session. A tool
// phase's process and a command unit's argv render nothing, so the fake must
// not ack for them either — an ack there would let a mixed wave's command unit
// settle a feedback debt only its agent siblings can render.
func sendsPrompt(request RunRequest) bool {
	if request.Key.UnitID == "" {
		return request.Phase.Driver == def.DriverAgent
	}
	unit := def.Unit{}
	if request.Unit != nil {
		unit = *request.Unit
	}
	driver, ok := unit.EffectiveDriver()
	return ok && driver == def.DriverAgent
}

func (f *fakeRunner) Stop(_ context.Context, key RunKey) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, key)
	return append(json.RawMessage(nil), f.partials[runMapKey(key)]...), lookupByRun(f.stopErrs, key)
}

// StopForTakeover records separately from Stop: a takeover detaches a run
// without killing it, and tests assert that teardown does not then stop it.
func (f *fakeRunner) StopForTakeover(_ context.Context, key RunKey) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.takeoverKeys = append(f.takeoverKeys, key)
	return append(json.RawMessage(nil), f.partials[runMapKey(key)]...), lookupByRun(f.stopErrs, key)
}

func (f *fakeRunner) complete(t *testing.T, itemID string, outcome Outcome) {
	t.Helper()
	f.mu.Lock()
	key, ok := f.lastItemRun[itemID]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("item %q has no phase-level runner start", itemID)
	}
	f.completeRun(t, key, outcome)
}

// completeRun reports an outcome for one exact run key, which is how a fan-out
// test completes an individual unit or its join.
func (f *fakeRunner) completeRun(t *testing.T, key RunKey, outcome Outcome) {
	t.Helper()
	f.mu.Lock()
	callback := f.callbacks[runMapKey(key)]
	f.mu.Unlock()
	if callback == nil {
		t.Fatalf("run %s has no active runner callback", runMapKey(key))
	}
	callback(outcome)
}

func (f *fakeRunner) startedKeys() []RunKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]RunKey, 0, len(f.starts))
	for _, start := range f.starts {
		keys = append(keys, start.Key)
	}
	return keys
}

func (f *fakeRunner) startedUnitIDs() []string {
	ids := make([]string, 0)
	for _, key := range f.startedKeys() {
		if key.UnitID != "" {
			ids = append(ids, key.UnitID)
		}
	}
	return ids
}

func (f *fakeRunner) takeovers() []RunKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RunKey(nil), f.takeoverKeys...)
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

func (f *fakeRunner) stopped() []RunKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RunKey(nil), f.stops...)
}

// startFor returns the request one exact run key was started with, which is how
// a fan-out test inspects the variable context a unit or join received.
func (f *fakeRunner) startFor(t *testing.T, key RunKey) RunRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := len(f.starts) - 1; index >= 0; index-- {
		if f.starts[index].Key == key {
			return f.starts[index]
		}
	}
	t.Fatalf("run %s was never started", runMapKey(key))
	return RunRequest{}
}

// lastStartFor is the most recent PHASE-level start for one run. It exists for
// assertions whose attempt number depends on how an intervening park landed
// rather than on anything the test is asserting.
func (f *fakeRunner) lastStartFor(t *testing.T, itemID string) RunRequest {
	t.Helper()
	f.mu.Lock()
	key, ok := f.lastItemRun[itemID]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("item %q has no phase-level runner start", itemID)
	}
	return f.startFor(t, key)
}

type emittedEvent struct {
	name    string
	payload any
}

type fakeEmitter struct {
	mu     sync.Mutex
	events []emittedEvent
}

func (f *fakeEmitter) Emit(name eventchan.Channel, payload any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, emittedEvent{name: string(name), payload: payload})
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

func (f *fakeEmitter) engineStateEvents() []EngineState {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []EngineState
	for _, event := range f.events {
		state, ok := event.payload.(EngineState)
		if event.name == "workflow:engine-state" && ok {
			result = append(result, state)
		}
	}
	return result
}

type testHarness struct {
	store       *store.Store
	engine      *Engine
	runner      *fakeRunner
	emitter     *fakeEmitter
	profiles    *fakeProfiles
	spend       *fakeSpendSource
	definitions *fakeDefinitions
}

// harnessOptions configures a test engine. `capacities` is applied to the
// project profiles before Start, so a test can make the rebuild itself run
// under a specific resource bound.
type harnessOptions struct {
	config     Config
	workflows  map[string]def.Workflow
	projectIDs []string
	capacities map[string]map[string]int
	// wrapStore interposes on the engine's persistence handle only; the harness
	// keeps the real store for its own assertions, so a test can fail one call
	// and still read what the run record actually holds.
	wrapStore   func(persistence) persistence
	beforeStart func(*store.Store)
	// replyBudget shortens the bound on how long an API call's reply waits for
	// the runner starts it produced. It is applied before Start, so nothing ever
	// reads the field while a test writes it.
	replyBudget time.Duration
}

func newHarness(t *testing.T, config Config, workflows map[string]def.Workflow, projectIDs []string, beforeStart func(*store.Store)) *testHarness {
	t.Helper()
	return newHarnessWith(t, harnessOptions{
		config: config, workflows: workflows, projectIDs: projectIDs, beforeStart: beforeStart,
	})
}

func newHarnessWith(t *testing.T, options harnessOptions) *testHarness {
	t.Helper()
	database, err := store.New(filepath.Join(t.TempDir(), "engine.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for index, projectID := range options.projectIDs {
		if err := database.CreateProject(store.Project{
			ID: projectID, Path: filepath.Join(t.TempDir(), projectID), Name: projectID,
			SortPosition: index, CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	if options.beforeStart != nil {
		options.beforeStart(database)
	}
	runner := newFakeRunner()
	emitter := &fakeEmitter{}
	profiles := &fakeProfiles{profiles: make(map[string]*profile.Profile)}
	spend := &fakeSpendSource{store: database, spends: make(map[string]Spend), errs: make(map[string]error)}
	for _, projectID := range options.projectIDs {
		capacities := make(map[string]int, len(options.capacities[projectID]))
		for name, capacity := range options.capacities[projectID] {
			capacities[name] = capacity
		}
		profiles.profiles[projectID] = &profile.Profile{Capacities: capacities}
	}
	definitions := &fakeDefinitions{workflows: options.workflows}
	var handle persistence = database
	if options.wrapStore != nil {
		handle = options.wrapStore(handle)
	}
	engine, err := New(handle, runner, emitter, definitions, profiles, spend, options.config)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	engine.now = func() time.Time { return time.UnixMilli(100) }
	if options.replyBudget > 0 {
		engine.startReplyBudget = options.replyBudget
	}
	runner.ack = engine.AckFeedbackRendered
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
	return &testHarness{
		store: database, engine: engine, runner: runner, emitter: emitter,
		profiles: profiles, spend: spend, definitions: definitions,
	}
}

// testItem builds an admissible run record. `ordinal` only spaces created_at
// apart so list order is deterministic; there is no queue rank to set.
func testItem(id, projectID, workflowID string, ordinal int) store.WorkItem {
	return store.WorkItem{
		ID: id, ProjectID: projectID, Goal: id, WorkflowID: workflowID,
		WorkflowScope: "shared", State: string(StateRunning),
		Seeds: json.RawMessage(`{}`), Source: "manual", CreatedAt: int64(ordinal + 10),
	}
}

func onePhaseWorkflow(id string, resources []string, routes []def.Route) def.Workflow {
	return def.Workflow{ID: id, Phases: []def.Phase{agentPhase("work", resources, routes)}}
}

// agentPhase mirrors what def validation guarantees for an agent phase: a
// provider is always present, which is what the implicit provider resource is
// keyed on.
func agentPhase(id string, resources []string, routes []def.Route) def.Phase {
	return def.Phase{
		ID: id, Driver: def.DriverAgent, Provider: testProvider, Model: "test-model",
		Resources: resources,
		Outputs:   map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}},
		Gate:      def.Gate{Routes: routes},
	}
}

const testProvider = "claude"

// staticFanOutPhase builds the fan-out shape the engine schedules: a phase that
// runs no turn of its own, `width` agent units, and the join whose envelope
// becomes the phase's.
//
// The phase keeps the agent driver/provider `agentPhase` set even though `def`
// validation now refuses those fields on a fan-out. That is deliberate: frozen
// snapshots are decoded, never re-validated (`decodeSnapshot`), so a run
// started before the rule landed still reaches the engine shaped like this. The
// shape guards it relies on — `phaseResources` skipping the provider bound,
// `PhaseProducesToolEnvelope` answering from the join — are only exercised by a
// fixture that carries the fields they have to ignore.
func staticFanOutPhase(id string, width int, resources []string, routes []def.Route) def.Phase {
	phase := agentPhase(id, resources, routes)
	phase.Shape = def.ShapeFanOut
	for index := 0; index < width; index++ {
		phase.FanOut = append(phase.FanOut, def.Unit{
			ID:       fmt.Sprintf("%s-unit-%d", id, index),
			Provider: testProvider, Model: "test-model", Prompt: "unit.md",
		})
	}
	phase.Join = &def.Unit{ID: id + "-join", Provider: testProvider, Model: "test-model", Prompt: "join.md"}
	return phase
}

// dynamicFanOutPhase fans out one template over an array variable, which is the
// authoring form whose width is only known at phase entry.
func dynamicFanOutPhase(id, over, as string, routes []def.Route) def.Phase {
	phase := agentPhase(id, nil, routes)
	phase.Shape = def.ShapeFanOut
	phase.Over = over
	phase.As = as
	phase.Unit = &def.Unit{
		ID: id + "-unit", Provider: testProvider, Model: "test-model",
		Prompt: "unit.md", Access: def.AccessWrite,
	}
	phase.Join = &def.Unit{ID: id + "-join", Provider: testProvider, Model: "test-model", Prompt: "join.md"}
	return phase
}

func fanOutWorkflow(id string, width int) def.Workflow {
	return def.Workflow{ID: id, Phases: []def.Phase{
		staticFanOutPhase("work", width, nil, []def.Route{{To: "done"}}),
	}}
}

func unitKey(itemID, phaseID string, attempt int, unitID string) RunKey {
	return RunKey{ItemID: itemID, PhaseID: phaseID, Attempt: attempt, UnitID: unitID}
}

// unitStatuses projects one attempt's persisted unit rows to `id -> status`,
// which is the fact every fan-out assertion is really about.
func (h *testHarness) unitStatuses(t *testing.T, itemID, phaseID string, attempt int) map[string]string {
	t.Helper()
	rows, err := h.store.ListWorkItemPhaseUnits(itemID, phaseID, attempt)
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]string, len(rows))
	for _, row := range rows {
		statuses[row.UnitID] = row.Status
	}
	return statuses
}

func (h *testHarness) requireUnitStatuses(t *testing.T, itemID, phaseID string, attempt int, want map[string]string) {
	t.Helper()
	got := h.unitStatuses(t, itemID, phaseID, attempt)
	if len(got) != len(want) {
		t.Fatalf("unit statuses = %v, want %v", got, want)
	}
	for id, status := range want {
		if got[id] != status {
			t.Fatalf("unit %q = %q, want %q (all: %v)", id, got[id], status, got)
		}
	}
}

// requireNoHeldResources asserts the teardown contract's observable outcome:
// after a run settles, no project resource is still checked out and nothing is
// left queued for one.
func (h *testHarness) requireNoHeldResources(t *testing.T) {
	t.Helper()
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if len(h.engine.holders) != 0 {
		t.Fatalf("resource holders = %v, want none", h.engine.holders)
	}
	if len(h.engine.waiting) != 0 || len(h.engine.waitingKeys) != 0 {
		t.Fatalf("waiting = %d entries / %d keys, want none", len(h.engine.waiting), len(h.engine.waitingKeys))
	}
}

// limitProviderCapacity pins the implicit provider resource to `capacity` so a
// test can make phase contention deterministic instead of relying on
// DefaultProviderCapacity.
func (h *testHarness) limitProviderCapacity(projectID string, capacity int) {
	h.profiles.setCapacity(projectID, ProviderResource(testProvider), capacity)
}

// limitFanOutWidth pins the project's absolute fan-out ceiling so a test can
// exercise the refusal at a small width instead of depending on
// def.DefaultMaxFanOutWidth.
func (h *testHarness) limitFanOutWidth(projectID string, width int) {
	h.profiles.setMaxFanOutWidth(projectID, width)
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

// requireParkCause asserts an engine-diagnosed park recorded its own diagnosis
// on the attempt row, and that it did NOT write one into the output envelope.
// The second half is the point: the envelope is the AGENT's artifact, and these
// parks happened without a turn ever running, so anything there would be engine
// prose every reader treats as something a model said.
func requireParkCause(t *testing.T, phase store.WorkItemPhase, wants ...string) {
	t.Helper()
	if phase.Status != "parked" {
		t.Fatalf("attempt %s/%d = status %q, want parked", phase.PhaseID, phase.Attempt, phase.Status)
	}
	if len(phase.OutputEnvelope) != 0 {
		t.Fatalf("engine park forged an agent envelope: %s", phase.OutputEnvelope)
	}
	for _, want := range wants {
		if !strings.Contains(phase.ParkCause, want) {
			t.Fatalf("park cause %q does not state %q", phase.ParkCause, want)
		}
	}
}
