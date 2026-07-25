package scheduler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"agent-overflow/internal/workflow/def"
)

// Kind is what caused a fire. Only Cron and Event are storable trigger kinds;
// Manual is the kind a Run-now fire reports in its variable context, so a phase
// can tell a human's press apart from its schedule.
type Kind string

const (
	KindCron   Kind = "cron"
	KindEvent  Kind = "event"
	KindManual Kind = "manual"
)

// ItemEventKind is the closed set of internal lifecycle events an automation
// may trigger on (§11). It is deliberately not "every event the system emits":
// each member is a run coming to rest, which is the only transition another run
// can meaningfully react to.
type ItemEventKind string

const (
	EventItemDone       ItemEventKind = "item-done"
	EventItemFailed     ItemEventKind = "item-failed"
	EventItemNeedsHuman ItemEventKind = "item-needs-human"
)

// EventKindForState maps a run's resting state onto the closed event set, and
// reports false for a state the set does not name. `cancelled` is deliberately
// absent: a human stopping a run is not a lifecycle result another run should
// chain off.
func EventKindForState(state string) (ItemEventKind, bool) {
	switch state {
	case "done":
		return EventItemDone, true
	case "failed":
		return EventItemFailed, true
	case "needs-human":
		return EventItemNeedsHuman, true
	default:
		return "", false
	}
}

// cronFieldCount is the standard five-field cron layout (minute, hour,
// day-of-month, month, day-of-week). The parser this package uses also accepts
// `@daily`-style descriptors and `@every <duration>`; both are refused before
// it sees them, because a trigger whose granularity can be sub-minute would
// make the one timer this package owns spin, and one grammar is easier to
// document on an automation row than three.
const cronFieldCount = 5

// Trigger is a parsed automation trigger. The stored JSON is one of:
//
//	{"kind":"cron","expr":"0 3 * * *"}
//	{"kind":"event","on":"item-done"}
//	{"kind":"event","on":"item-failed","workflowId":"nightly-audit"}
//
// Unknown fields are refused rather than ignored: a typo in a stored trigger
// must be visible when it is written, not silently disable a schedule.
type Trigger struct {
	Kind Kind   `json:"kind"`
	Expr string `json:"expr,omitempty"`
	// On and WorkflowID belong to an event trigger. An empty WorkflowID matches
	// every workflow in the automation's project.
	On         ItemEventKind `json:"on,omitempty"`
	WorkflowID string        `json:"workflowId,omitempty"`

	// schedule is the compiled cron spec. It is derived, so it is unexported and
	// never round-trips through JSON; ParseTrigger is the only way to obtain a
	// Trigger that can compute a next fire.
	schedule cron.Schedule
}

// ParseTrigger validates a stored trigger and compiles its schedule. Every
// caller parses: an automation is validated on write and again on load, so a
// definition that stopped parsing (hand-edited row, older shape) surfaces as
// broken on its row rather than as a schedule that quietly never fires.
func ParseTrigger(raw json.RawMessage) (Trigger, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Trigger{}, fmt.Errorf("trigger is required")
	}
	var trigger Trigger
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&trigger); err != nil {
		return Trigger{}, fmt.Errorf("trigger must be an object of the documented shape: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Trigger{}, fmt.Errorf("trigger must contain one JSON object")
	}
	trigger.Expr = strings.TrimSpace(trigger.Expr)
	trigger.WorkflowID = strings.TrimSpace(trigger.WorkflowID)
	switch trigger.Kind {
	case KindCron:
		if trigger.On != "" || trigger.WorkflowID != "" {
			return Trigger{}, fmt.Errorf("a cron trigger declares expr only")
		}
		schedule, err := parseCron(trigger.Expr)
		if err != nil {
			return Trigger{}, err
		}
		trigger.schedule = schedule
	case KindEvent:
		if trigger.Expr != "" {
			return Trigger{}, fmt.Errorf("an event trigger declares on (and optionally workflowId) only")
		}
		switch trigger.On {
		case EventItemDone, EventItemFailed, EventItemNeedsHuman:
		default:
			return Trigger{}, fmt.Errorf(
				"event trigger on must be one of %s, %s, %s",
				EventItemDone, EventItemFailed, EventItemNeedsHuman,
			)
		}
	case "":
		return Trigger{}, fmt.Errorf("trigger kind is required (%s or %s)", KindCron, KindEvent)
	default:
		return Trigger{}, fmt.Errorf("trigger kind %q must be %s or %s", trigger.Kind, KindCron, KindEvent)
	}
	return trigger, nil
}

func parseCron(expr string) (cron.Schedule, error) {
	if expr == "" {
		return nil, fmt.Errorf("a cron trigger requires expr")
	}
	if fields := strings.Fields(expr); len(fields) != cronFieldCount {
		return nil, fmt.Errorf(
			"cron expr %q has %d fields; expected %d (minute hour day-of-month month day-of-week)",
			expr, len(fields), cronFieldCount,
		)
	}
	schedule, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("cron expr %q is invalid: %w", expr, err)
	}
	return schedule, nil
}

// Next returns the first fire strictly after `after`, and false for a trigger
// that has no schedule of its own (an event trigger fires when the system says
// so, not when the clock does).
func (t Trigger) Next(after time.Time) (time.Time, bool) {
	if t.Kind != KindCron || t.schedule == nil {
		return time.Time{}, false
	}
	next := t.schedule.Next(after)
	if next.IsZero() {
		return time.Time{}, false
	}
	return next, true
}

// Summary is the one-line human rendering used on an automation row and in the
// goal of every run it starts.
func (t Trigger) Summary() string {
	switch t.Kind {
	case KindCron:
		return "cron " + t.Expr
	case KindEvent:
		if t.WorkflowID != "" {
			return fmt.Sprintf("event %s on %s", t.On, t.WorkflowID)
		}
		return "event " + string(t.On)
	default:
		return string(t.Kind)
	}
}

// ItemEvent is one root run's resting transition, as the app observes it. The
// scheduler derives the event kind from State rather than being told it, so the
// closed set has exactly one definition.
type ItemEvent struct {
	ProjectID  string `json:"projectId"`
	ItemID     string `json:"itemId"`
	WorkflowID string `json:"workflowId"`
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	// ParentItemID is empty for a root run. A called run's transitions are its
	// tree's internals: matching them would let one run tree fan a chain storm,
	// so only roots are matched.
	ParentItemID string `json:"parentItemId,omitempty"`
	// Source and SourceRef are the transitioning run's own provenance, which is
	// what the self-chain guard reads.
	Source    string `json:"source"`
	SourceRef string `json:"sourceRef,omitempty"`
}

// Matches reports whether an event trigger fires for this event: same project,
// a named kind, and — when the trigger narrows it — the same workflow.
func (t Trigger) Matches(automationProjectID string, event ItemEvent) bool {
	if t.Kind != KindEvent || event.ParentItemID != "" {
		return false
	}
	kind, ok := EventKindForState(event.State)
	if !ok || kind != t.On {
		return false
	}
	if event.ProjectID != automationProjectID {
		return false
	}
	return t.WorkflowID == "" || t.WorkflowID == event.WorkflowID
}

// Fire is one trigger occurrence: what fired, when, and — for an event trigger
// — the run transition that caused it.
type Fire struct {
	Kind Kind
	At   time.Time
	// ScheduledFor is the cron occurrence this fire belongs to, which can differ
	// from At by the scheduler's wake latency. A phase that reasons about "which
	// tick am I" wants the schedule, not the wall clock.
	ScheduledFor time.Time
	Event        *ItemEvent
}

// Summary renders the fire for the started run's goal.
func (f Fire) Summary() string {
	switch f.Kind {
	case KindCron:
		return "cron " + f.ScheduledFor.Format(time.RFC3339)
	case KindEvent:
		if f.Event != nil {
			kind, _ := EventKindForState(f.Event.State)
			return fmt.Sprintf("event %s from run %s", kind, f.Event.ItemID)
		}
		return "event"
	default:
		return "run now"
	}
}

// Context is the value of the reserved `trigger` variable a started run is
// seeded with. Keys are kebab-case because that is the only identifier grammar
// `def` can reference: a phase reads `{{trigger.fired-at}}` and a run-if
// condition resolves `trigger.event.state` through the same lookup.
func (f Fire) Context() map[string]any {
	context := map[string]any{
		"kind":     string(f.Kind),
		"fired-at": f.At.UnixMilli(),
	}
	if f.Kind == KindCron {
		context["scheduled-for"] = f.ScheduledFor.UnixMilli()
	}
	if f.Event != nil {
		kind, _ := EventKindForState(f.Event.State)
		context["event"] = map[string]any{
			"kind":        string(kind),
			"item-id":     f.Event.ItemID,
			"workflow-id": f.Event.WorkflowID,
			"state":       f.Event.State,
			"reason":      f.Event.Reason,
		}
	}
	return context
}

// The reserved seed names every fired run carries. Reserved means an automation
// may not store a seed under either name (creation and update refuse it) and
// the scheduler binds them last, so nothing can shadow them — the same rule
// `def` applies to a join's `units`.
//
// Both are kebab-case for the reason above: `job_notes` could never be declared
// as a workflow input or referenced from a prompt, so it would be an inert seed.
const (
	TriggerVariable  = "trigger"
	JobNotesVariable = "job-notes"
)

// ReservedSeed reports whether a name belongs to the scheduler.
func ReservedSeed(name string) bool {
	return name == TriggerVariable || name == JobNotesVariable
}

// ParseCondition decodes an automation's optional run-if condition. Absent is
// legal and reported by the boolean; present-but-malformed is an error, at both
// write time and fire time.
func ParseCondition(raw json.RawMessage) (def.Predicate, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return def.Predicate{}, false, nil
	}
	var predicate def.Predicate
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&predicate); err != nil {
		return def.Predicate{}, false, fmt.Errorf("condition must be a predicate object: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return def.Predicate{}, false, fmt.Errorf("condition must contain one JSON object")
	}
	if findings := def.ValidatePredicateShape(predicate, "condition"); len(findings) > 0 {
		messages := make([]string, 0, len(findings))
		for _, finding := range findings {
			messages = append(messages, finding.Message)
		}
		return def.Predicate{}, false, fmt.Errorf("condition is malformed: %s", strings.Join(messages, "; "))
	}
	return predicate, true, nil
}
