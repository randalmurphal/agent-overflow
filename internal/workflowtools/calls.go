package workflowtools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/workflow/def"
)

// call is one tool call's view: the app and the authority it runs under.
type call struct {
	app    App
	caller Caller
	now    func() time.Time
}

func (c *call) workflowList(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct{}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	listing, err := c.app.ListWorkflows(ctx)
	return listing, failed(err)
}

func (c *call) workflowValidate(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	id, path := strings.TrimSpace(args.ID), strings.TrimSpace(args.Path)
	if (id == "") == (path == "") {
		return nil, invalidf("Name exactly one of id and path.")
	}
	if path != "" && !filepath.IsAbs(path) {
		return nil, invalidf("path must be absolute.")
	}
	result, err := c.app.ValidateWorkflow(ctx, id, path)
	return result, failed(err)
}

func (c *call) workflowSchema(raw json.RawMessage) (any, error) {
	var args struct{}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	return json.RawMessage(def.AuthoringSchema()), nil
}

func (c *call) workflowNew(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Starter string `json:"starter"`
		ID      string `json:"id"`
		Scope   string `json:"scope"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("starter", args.Starter); err != nil {
		return nil, err
	}
	if err := required("id", args.ID); err != nil {
		return nil, err
	}
	if args.Scope != "project" && args.Scope != "shared" {
		return nil, invalidf("scope must be project or shared.")
	}
	result, err := c.app.ScaffoldWorkflow(ctx, args.Starter, strings.TrimSpace(args.ID), args.Scope == "project")
	return result, failed(err)
}

// waitArgs is the wait half every waiting tool shares.
type waitArgs struct {
	WaitSeconds int  `json:"wait_seconds"`
	Notify      bool `json:"notify"`
}

func (w waitArgs) validate() error {
	if w.WaitSeconds < 0 || w.WaitSeconds > MaxWaitSeconds {
		return invalidf("wait_seconds must be between 0 and %d.", MaxWaitSeconds)
	}
	return nil
}

// Outcomes of a waiting call. Unknown is a run_start whose run started but
// could not be read back; the start document still names it.
const (
	OutcomeResting      = "resting"
	OutcomeRunning      = "running"
	OutcomeBackgrounded = "backgrounded"
	OutcomeUnknown      = "unknown"
)

// Notify says whether a run's rest will reach the caller's thread.
type Notify struct {
	Armed  bool   `json:"armed"`
	Detail string `json:"detail"`
}

// Wait is the answer to a waiting call: the run's status document, where
// the wait left it, and, when it matters, whether its rest will reach the
// caller. ReadError says why a run that started could not be read back.
type Wait struct {
	Run       any     `json:"run,omitempty"`
	Outcome   string  `json:"outcome"`
	Notify    *Notify `json:"notify,omitempty"`
	ReadError string  `json:"readError,omitempty"`
}

// StartResult is run_start's answer: the start document, and the wait when
// one was asked for.
type StartResult struct {
	Start any `json:"start"`
	Wait
}

func (c *call) runStart(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		WorkflowID string          `json:"workflow_id"`
		Scope      string          `json:"scope"`
		Goal       string          `json:"goal"`
		Seeds      json.RawMessage `json:"seeds"`
		BaseBranch string          `json:"base_branch"`
		StepMode   bool            `json:"step_mode"`
		waitArgs
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("workflow_id", args.WorkflowID); err != nil {
		return nil, err
	}
	if args.Scope != "" && args.Scope != "project" && args.Scope != "shared" {
		return nil, invalidf("scope must be project or shared.")
	}
	if err := checkObject("seeds", args.Seeds); err != nil {
		return nil, err
	}
	// Everything is checked before the start: once StartRun commits, the run
	// exists, and the answer must name it whatever happens after.
	if err := args.validate(); err != nil {
		return nil, err
	}
	started, err := c.app.StartRun(ctx, StartInput{
		WorkflowID: strings.TrimSpace(args.WorkflowID), Scope: args.Scope, Goal: args.Goal,
		Seeds: args.Seeds, BaseBranch: args.BaseBranch, StepMode: args.StepMode,
		HoldWake: c.holdsWakes(args.waitArgs),
	})
	if err != nil {
		return nil, failed(err)
	}
	var returned *Rest
	if started.Hold != nil {
		defer func() { started.Hold.Release(returned) }()
	}
	if args.WaitSeconds == 0 && !args.Notify {
		return struct {
			Start any `json:"start"`
		}{started.Result}, nil
	}
	wait, rest, err := c.wait(ctx, started.RunID, args.waitArgs)
	if err != nil {
		// The run started; a failed read must not read as a failed start, or
		// the caller starts it again. An interactive start has no
		// surface-and-skip to catch the second one.
		return StartResult{Start: started.Result, Wait: Wait{Outcome: OutcomeUnknown, ReadError: publicMessage(err)}}, nil
	}
	returned = rest
	return StartResult{Start: started.Result, Wait: wait}, nil
}

// holdsWakes reports whether a call that waits as args asks holds the run's
// wake while it does. Only an interactive caller can be woken, and only a
// call that waits can deliver a rest in the wake's place.
func (c *call) holdsWakes(args waitArgs) bool {
	return args.WaitSeconds > 0 && !c.caller.Phase
}

// publicMessage is the text a caller may read for an app refusal.
func publicMessage(err error) string {
	_, message, _ := errorsx.PublicDetails(failed(err))
	return message
}

func (c *call) runStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		RunID string `json:"run_id"`
		waitArgs
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("run_id", args.RunID); err != nil {
		return nil, err
	}
	if err := args.validate(); err != nil {
		return nil, err
	}
	runID := strings.TrimSpace(args.RunID)
	var returned *Rest
	if c.holdsWakes(args.waitArgs) {
		hold := c.app.HoldWake(ctx, runID)
		defer func() { hold.Release(returned) }()
	}
	wait, rest, err := c.wait(ctx, runID, args.waitArgs)
	if err != nil {
		return nil, err
	}
	returned = rest
	return wait, nil
}

// wait blocks until the run rests or the wait passes, then reads its status.
// The wait is spent in the app's own bounded holds, so a client that goes away
// ends it at the next hold rather than at the deadline. A wait that ends at
// rest also names the rest it read.
func (c *call) wait(ctx context.Context, runID string, args waitArgs) (Wait, *Rest, error) {
	resting := false
	var rest *Rest
	if args.WaitSeconds > 0 {
		deadline := c.clock().Add(time.Duration(args.WaitSeconds) * time.Second)
		var cursor int64
		for {
			watched, err := c.app.WatchRun(ctx, WatchInput{RunID: runID, Cursor: cursor, Wait: deadline.Sub(c.clock())})
			if err != nil {
				return Wait{}, nil, failed(err)
			}
			cursor = watched.Cursor
			resting = watched.Resting
			if resting {
				rest = &Rest{Cursor: watched.Cursor, State: watched.State, Reason: watched.Reason}
				break
			}
			if !c.clock().Before(deadline) {
				break
			}
			if err := ctx.Err(); err != nil {
				return Wait{}, nil, failed(err)
			}
		}
	}
	status, err := c.app.RunStatus(ctx, runID)
	if err != nil {
		return Wait{}, nil, failed(err)
	}
	if args.WaitSeconds == 0 {
		// No wait was spent, so the state comes from the status read alone.
		resting = statusResting(status)
	}
	result := Wait{Run: status, Outcome: OutcomeRunning}
	switch {
	case resting:
		result.Outcome = OutcomeResting
	case args.WaitSeconds > 0:
		result.Outcome = OutcomeBackgrounded
	}
	if result.Outcome == OutcomeBackgrounded || (args.Notify && result.Outcome != OutcomeResting) {
		// The status is already read; a failed binding lookup costs the
		// caller the notify answer, not the status.
		notify, err := c.notify(ctx, runID)
		if err != nil {
			notify = Notify{Detail: fmt.Sprintf(notifyUnreadableF, publicMessage(err))}
		}
		result.Notify = &notify
	}
	return result, rest, nil
}

// statusResting reads the resting flag off the app's status document. The
// document is the app's; this is the one field the tool reads back.
func statusResting(status any) bool {
	encoded, err := json.Marshal(status)
	if err != nil {
		return false
	}
	var view struct {
		Resting bool `json:"resting"`
	}
	if err := json.Unmarshal(encoded, &view); err != nil {
		return false
	}
	return view.Resting
}

// The three notify answers. A run's rest reaches a thread only through its
// binding: the bound thread is woken whenever the run comes to rest (D17).
// Nothing here binds a run; binding is the overlay's, or the start's.
const (
	notifyBound       = "This run is bound to your thread: when it comes to rest, a message naming what to do next arrives here at your next turn boundary. You do not need to poll."
	notifyUnbound     = "This run is not bound to your thread; poll run_status again or bind it from the overlay."
	notifyPhase       = "A workflow phase session receives no wakes; call run_status again to read where the run stands."
	notifyElsewhereF  = "This run reports to thread %s, not to yours; poll run_status again."
	notifyUnreadableF = "Whether this run's rest reaches your thread could not be read (%s); poll run_status again."
)

func (c *call) notify(ctx context.Context, runID string) (Notify, error) {
	if c.caller.Phase {
		return Notify{Detail: notifyPhase}, nil
	}
	bound, err := c.app.RunBoundThread(ctx, runID)
	if err != nil {
		return Notify{}, failed(err)
	}
	switch bound {
	case c.caller.ThreadID:
		return Notify{Armed: true, Detail: notifyBound}, nil
	case "":
		return Notify{Detail: notifyUnbound}, nil
	default:
		return Notify{Detail: fmt.Sprintf(notifyElsewhereF, bound)}, nil
	}
}

func (c *call) runWatch(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		RunID       string `json:"run_id"`
		Cursor      int64  `json:"cursor"`
		Tree        bool   `json:"tree"`
		WaitSeconds *int   `json:"wait_seconds"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("run_id", args.RunID); err != nil {
		return nil, err
	}
	if args.Cursor < 0 {
		return nil, invalidf("cursor must not be negative.")
	}
	waitSeconds := 60
	if args.WaitSeconds != nil {
		waitSeconds = *args.WaitSeconds
	}
	if waitSeconds < 0 || waitSeconds > MaxWaitSeconds {
		return nil, invalidf("wait_seconds must be between 0 and %d.", MaxWaitSeconds)
	}
	deadline := c.clock().Add(time.Duration(waitSeconds) * time.Second)
	input := WatchInput{RunID: strings.TrimSpace(args.RunID), Cursor: args.Cursor, Tree: args.Tree}
	for {
		input.Wait = deadline.Sub(c.clock())
		watched, err := c.app.WatchRun(ctx, input)
		if err != nil {
			return nil, failed(err)
		}
		// A first call answers with where the run stands; every later one
		// answers as soon as something moved or the run is resting.
		if input.Cursor == 0 || watched.Transitions > 0 || watched.Resting || !c.clock().Before(deadline) || ctx.Err() != nil {
			return watched.Result, nil
		}
		input.Cursor = watched.Cursor
	}
}

func (c *call) runOutput(ctx context.Context, raw json.RawMessage) (any, error) {
	runID, err := decodeRunID(raw)
	if err != nil {
		return nil, err
	}
	result, err := c.app.RunOutput(ctx, runID)
	return result, failed(err)
}

func (c *call) runInspect(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		RunID   string `json:"run_id"`
		PhaseID string `json:"phase_id"`
		Attempt int    `json:"attempt"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("run_id", args.RunID); err != nil {
		return nil, err
	}
	if args.Attempt < 0 {
		return nil, invalidf("attempt must be positive.")
	}
	if args.Attempt != 0 && strings.TrimSpace(args.PhaseID) == "" {
		return nil, invalidf("attempt reads an older attempt of phase_id; name the phase.")
	}
	result, err := c.app.InspectRun(ctx, InspectInput{RunID: strings.TrimSpace(args.RunID), PhaseID: strings.TrimSpace(args.PhaseID), Attempt: args.Attempt})
	return result, failed(err)
}

func (c *call) runNarrative(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		RunID   string `json:"run_id"`
		PhaseID string `json:"phase_id"`
		Attempt int    `json:"attempt"`
		UnitID  string `json:"unit_id"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("run_id", args.RunID); err != nil {
		return nil, err
	}
	if err := required("phase_id", args.PhaseID); err != nil {
		return nil, err
	}
	if args.Attempt < 0 {
		return nil, invalidf("attempt must be positive.")
	}
	result, err := c.app.RunNarrative(ctx, NarrativeInput{
		RunID: strings.TrimSpace(args.RunID), PhaseID: strings.TrimSpace(args.PhaseID),
		Attempt: args.Attempt, UnitID: strings.TrimSpace(args.UnitID),
	})
	return result, failed(err)
}

func (c *call) runList(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ActiveOnly bool `json:"active_only"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	result, err := c.app.ListRuns(ctx, args.ActiveOnly)
	return result, failed(err)
}

// controlFields is which optional run_control fields each action takes. A
// field set for an action that does not take it is refused by name rather
// than ignored.
var controlFields = map[string][]string{
	"pause":              nil,
	"cancel":             nil,
	"resume":             {"phase_id", "refresh_def", "at"},
	"rerun":              {"guidance", "refresh_def"},
	"soft_stop":          {"clear"},
	"retry_unit":         {"unit_id", "note"},
	"retry_failed_units": {"note"},
}

func (c *call) runControl(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		RunID      string `json:"run_id"`
		Action     string `json:"action"`
		PhaseID    string `json:"phase_id"`
		RefreshDef bool   `json:"refresh_def"`
		At         string `json:"at"`
		Guidance   string `json:"guidance"`
		UnitID     string `json:"unit_id"`
		Note       string `json:"note"`
		Clear      bool   `json:"clear"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("run_id", args.RunID); err != nil {
		return nil, err
	}
	allowed, ok := controlFields[args.Action]
	if !ok {
		return nil, invalidf("action must be one of %s.", joinNames(ControlActions))
	}
	set := map[string]bool{
		"phase_id": strings.TrimSpace(args.PhaseID) != "", "refresh_def": args.RefreshDef,
		"at": strings.TrimSpace(args.At) != "", "guidance": strings.TrimSpace(args.Guidance) != "",
		"unit_id": strings.TrimSpace(args.UnitID) != "", "note": strings.TrimSpace(args.Note) != "",
		"clear": args.Clear,
	}
	for _, field := range []string{"phase_id", "refresh_def", "at", "guidance", "unit_id", "note", "clear"} {
		if set[field] && !slices.Contains(allowed, field) {
			return nil, invalidf("%s does not apply to %s.", field, args.Action)
		}
	}
	if args.Action == "resume" && set["at"] && (set["phase_id"] || set["refresh_def"]) {
		return nil, invalidf("at schedules the bare resume; it does not take phase_id or refresh_def.")
	}
	if args.Action == "retry_unit" && !set["unit_id"] {
		return nil, invalidf("retry_unit needs unit_id; run_status lists the failed units.")
	}
	runID := strings.TrimSpace(args.RunID)
	done, err := c.app.ControlRun(ctx, ControlInput{
		RunID: runID, Action: args.Action, PhaseID: strings.TrimSpace(args.PhaseID),
		RefreshDef: args.RefreshDef, At: strings.TrimSpace(args.At), Guidance: args.Guidance,
		UnitID: strings.TrimSpace(args.UnitID), Note: args.Note, Clear: args.Clear,
	})
	if err != nil {
		return nil, failed(err)
	}
	// What state the run is in now is the useful answer to acting on it, but
	// the action is done whether or not that read succeeds.
	status, err := c.app.RunStatus(ctx, runID)
	if err != nil {
		return ActionDone{RunID: runID, Action: args.Action, Done: true, ArmedAt: done.ArmedAt, ReadError: publicMessage(err)}, nil
	}
	if done.ArmedAt != "" {
		return ScheduledResume{ArmedAt: done.ArmedAt, Run: status}, nil
	}
	return status, nil
}

// ScheduledResume is run_control resume with at: the moment the resume armed,
// and the run, which stays where it is until then.
type ScheduledResume struct {
	ArmedAt string `json:"armedAt"`
	Run     any    `json:"run"`
}

// ActionDone answers an action that took effect when the run could not be
// read back afterwards.
type ActionDone struct {
	RunID     string `json:"itemId"`
	Action    string `json:"action"`
	Done      bool   `json:"done"`
	ArmedAt   string `json:"armedAt,omitempty"`
	ReadError string `json:"readError"`
}

func (c *call) runAmend(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		RunID string          `json:"run_id"`
		Seeds json.RawMessage `json:"seeds"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("run_id", args.RunID); err != nil {
		return nil, err
	}
	if len(args.Seeds) == 0 {
		return nil, invalidf("seeds is required: name at least one input to change.")
	}
	if err := checkObject("seeds", args.Seeds); err != nil {
		return nil, err
	}
	result, err := c.app.AmendSeeds(ctx, strings.TrimSpace(args.RunID), args.Seeds)
	return result, failed(err)
}

func (c *call) runGuide(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		RunID string `json:"run_id"`
		Text  string `json:"text"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("run_id", args.RunID); err != nil {
		return nil, err
	}
	if err := required("text", args.Text); err != nil {
		return nil, err
	}
	result, err := c.app.GuideRun(ctx, strings.TrimSpace(args.RunID), args.Text)
	return result, failed(err)
}

func (c *call) runResolve(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		RunID    string `json:"run_id"`
		Decision string `json:"decision"`
		Note     string `json:"note"`
		Answer   string `json:"answer"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("run_id", args.RunID); err != nil {
		return nil, err
	}
	switch args.Decision {
	case "approve", "reject":
		if strings.TrimSpace(args.Answer) != "" {
			return nil, invalidf("answer applies to decision answer; a gate takes note.")
		}
	case "answer":
		if err := required("answer", args.Answer); err != nil {
			return nil, err
		}
		if strings.TrimSpace(args.Note) != "" {
			return nil, invalidf("note applies to approve and reject; put everything in answer.")
		}
	default:
		return nil, invalidf("decision must be approve, reject or answer.")
	}
	runID := strings.TrimSpace(args.RunID)
	if err := c.app.ResolveRun(ctx, ResolveInput{
		RunID: runID, Decision: args.Decision, Note: args.Note, Answer: args.Answer,
	}); err != nil {
		return nil, failed(err)
	}
	status, err := c.app.RunStatus(ctx, runID)
	if err != nil {
		return ActionDone{RunID: runID, Action: args.Decision, Done: true, ReadError: publicMessage(err)}, nil
	}
	return status, nil
}

func (c *call) memoryAdd(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Kind  string   `json:"kind"`
		Text  string   `json:"text"`
		Files []string `json:"files"`
		RunID string   `json:"run_id"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("kind", args.Kind); err != nil {
		return nil, err
	}
	if err := required("text", args.Text); err != nil {
		return nil, err
	}
	result, err := c.app.AddMemory(ctx, MemoryInput{
		RunID: strings.TrimSpace(args.RunID), Kind: strings.TrimSpace(args.Kind), Text: args.Text, Files: args.Files,
	})
	return result, failed(err)
}

func (c *call) memoryList(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Kind  string `json:"kind"`
		RunID string `json:"run_id"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	result, err := c.app.ListMemory(ctx, strings.TrimSpace(args.RunID), strings.TrimSpace(args.Kind))
	return result, failed(err)
}

func decodeRunID(raw json.RawMessage) (string, error) {
	var args struct {
		RunID string `json:"run_id"`
	}
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	if err := required("run_id", args.RunID); err != nil {
		return "", err
	}
	return strings.TrimSpace(args.RunID), nil
}

// checkObject refuses a present value that is not a JSON object; seeds are
// keyed by input name.
func checkObject(field string, raw json.RawMessage) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if !strings.HasPrefix(trimmed, "{") {
		return invalidf("%s must be an object keyed by input name.", field)
	}
	return nil
}

func (c *call) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
