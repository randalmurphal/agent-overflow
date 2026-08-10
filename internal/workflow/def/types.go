package def

import (
	"fmt"
	"strings"
)

// Scope identifies where a workflow definition was loaded from.
type Scope string

const (
	ScopeShared  Scope = "shared"
	ScopeProject Scope = "project"
)

// Workflow is the complete portable workflow authoring format.
type Workflow struct {
	ID          string                    `yaml:"id" json:"id"`
	Name        string                    `yaml:"name" json:"name"`
	Description string                    `yaml:"description,omitempty" json:"description,omitempty"`
	Inputs      map[string]Variable       `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Outputs     map[string]WorkflowOutput `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	// NonGoals is the author's "do not drift here" list: work this definition
	// deliberately does not do, stated so every element of every run it produces
	// reads it alongside the goal the run was started with. It is DEF-owned
	// where the goal is RUN-owned — a non-goal is a property of the workflow,
	// not of one invocation of it — and it freezes with the snapshot like every
	// other authored field. See `goals.go` for the bounds and `PromptContext`
	// in `internal/workflow/runner` for how it reaches an element.
	NonGoals        []string      `yaml:"non_goals,omitempty" json:"nonGoals,omitempty"`
	Phases          []Phase       `yaml:"phases" json:"phases"`
	DefaultStepMode bool          `yaml:"default_step_mode,omitempty" json:"defaultStepMode,omitempty"`
	Cleanup         CleanupPolicy `yaml:"cleanup,omitempty" json:"cleanup,omitempty"`
}

// CleanupPolicy records the requested terminal-state worktree cleanup.
// Execution for writing workflows is deferred until disposition lands; v1
// keeps every unlanded worktree regardless of this value.
type CleanupPolicy string

const (
	CleanupManual CleanupPolicy = "manual"
	CleanupAuto   CleanupPolicy = "auto"
)

// Variable declares a JSON-Schema type and whether absence is permitted.
// Required is the default: optionality must be explicit.
type Variable struct {
	Schema   JSONSchema `yaml:"schema" json:"schema"`
	Optional bool       `yaml:"optional,omitempty" json:"optional,omitempty"`
	// Window bounds a reserved `history.<phase>` input binding to that many of
	// the phase's most recent prior attempts (DefaultHistoryWindow when
	// omitted, MaxHistoryWindow at the ceiling). It is legal on that binding
	// alone: every other declaration names one value rather than a series, so a
	// window there would configure nothing and is a validation finding.
	Window int `yaml:"window,omitempty" json:"window,omitempty"`
}

// WorkflowOutput names a run deliverable sourced from a phase output.
type WorkflowOutput struct {
	From string `yaml:"from" json:"from"`
	// Optional says this deliverable may be absent when the run completes.
	// A required one that its completion path never produced fails the run AT
	// the moment it finishes — the caller of a call edge synthesizes its
	// envelope from these names, and a name with no value is an error there.
	// That is correct for a deliverable a consumer routes on, and wrong for one
	// a branch of the graph simply does not produce: a campaign whose planner
	// reports `complete` on the exit path never runs the phase that sources its
	// per-wave handoff, so declaring it killed the campaign at the exact moment
	// it succeeded.
	//
	// Absence is spelled the way an absent optional call argument is (D45): the
	// name is OMITTED from the synthesized envelope rather than answered null,
	// so a child's optional output and a caller's optional input meet as the
	// same "not supplied" a direct start would have produced.
	Optional bool `yaml:"optional,omitempty" json:"optional,omitempty"`
	Artifact bool `yaml:"artifact,omitempty" json:"artifact,omitempty"`
}

// Phase is one graph node and its ordered exit routes.
type Phase struct {
	ID       string `yaml:"id" json:"id"`
	Name     string `yaml:"name,omitempty" json:"name,omitempty"`
	Driver   Driver `yaml:"driver" json:"driver"`
	Shape    Shape  `yaml:"shape,omitempty" json:"shape,omitempty"`
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	Model    string `yaml:"model,omitempty" json:"model,omitempty"`
	// Effort pins the reasoning tier of this phase's turn, from the closed
	// EffortTier vocabulary. It is legal exactly where provider/model are — on a
	// phase that runs its own agent turn — and empty means the model's catalog
	// default. A tier the model does not advertise is coerced at thread creation
	// rather than refused by validation; see effortTierFindings.
	Effort       string              `yaml:"effort,omitempty" json:"effort,omitempty"`
	Prompt       string              `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Watchdog     string              `yaml:"watchdog,omitempty" json:"watchdog,omitempty"`
	Inputs       map[string]Variable `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Outputs      map[string]Variable `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	Check        string              `yaml:"check,omitempty" json:"check,omitempty"`
	Command      string              `yaml:"command,omitempty" json:"command,omitempty"`
	Resources    []string            `yaml:"resources,omitempty" json:"resources,omitempty"`
	Commands     []string            `yaml:"commands,omitempty" json:"commands,omitempty"`
	Capabilities []string            `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	MCP          []string            `yaml:"mcp,omitempty" json:"mcp,omitempty"`
	// Grants are the first-party `ao` capabilities this phase's agent may use
	// (§5). They are frozen into the run snapshot with everything else, so a
	// phase re-entered after the definition changed keeps the authority the run
	// started with.
	Grants   []string          `yaml:"grants,omitempty" json:"grants,omitempty"`
	Access   Access            `yaml:"access,omitempty" json:"access,omitempty"`
	FanOut   []Unit            `yaml:"fan_out,omitempty" json:"fanOut,omitempty"`
	Over     string            `yaml:"over,omitempty" json:"over,omitempty"`
	As       string            `yaml:"as,omitempty" json:"as,omitempty"`
	Unit     *Unit             `yaml:"unit,omitempty" json:"unit,omitempty"`
	Join     *Unit             `yaml:"join,omitempty" json:"join,omitempty"`
	Call     string            `yaml:"call,omitempty" json:"call,omitempty"`
	Args     map[string]string `yaml:"args,omitempty" json:"args,omitempty"`
	MaxDepth int               `yaml:"max_depth,omitempty" json:"maxDepth,omitempty"`
	Gate     Gate              `yaml:"gate" json:"gate"`
}

// EffectiveShape resolves the phase's declared shape, defaulting to single.
func (p Phase) EffectiveShape() Shape {
	if p.Shape == "" {
		return ShapeSingle
	}
	return p.Shape
}

// DynamicFanOut reports whether the phase stamps its units from one template
// over an array variable instead of declaring them statically. Any of the three
// dynamic fields counts, so a half-authored dynamic form is validated as a
// dynamic fan-out (and told what is missing) rather than silently read as a
// static fan-out with no units.
func (p Phase) DynamicFanOut() bool {
	return p.Over != "" || p.As != "" || p.Unit != nil
}

// UnitDefinitions returns every unit definition the phase can run: its static
// list, or the single dynamic template. The join is not included — it is a
// distinct field with its own lifecycle.
func (p Phase) UnitDefinitions() []Unit {
	if p.DynamicFanOut() {
		if p.Unit == nil {
			return nil
		}
		return []Unit{*p.Unit}
	}
	return p.FanOut
}

type Driver string
type Shape string
type Access string

const (
	DriverAgent    Driver = "agent"
	DriverTool     Driver = "tool"
	ShapeSingle    Shape  = "single"
	ShapeFanOut    Shape  = "fan-out"
	ShapeCall      Shape  = "call"
	AccessReadOnly Access = "read-only"
	AccessWrite    Access = "write"
)

// Unit configures one fan-out worker or its required join.
//
// A unit carries exactly one of three bindings, and the one it carries is the
// discriminator for how it runs: `prompt` (with provider/model) runs an agent
// turn, `command` runs deterministically like a tool phase, and `call` invokes
// another workflow as a child run instead of running work of its own. The three
// are mutually exclusive and one is required; validation refuses every other
// combination, so nothing downstream has to guess.
//
// Outputs is the unit's own envelope contract, declared exactly like a phase's
// and never merged with one: a unit reports to its join, not to the gate. A
// unit that declares none returns the control-only envelope (status, question,
// reason), which is all a join reading branches and worktrees needs. The join
// itself declares none — its envelope IS the phase's, so it answers the
// phase's `outputs:` and declaring its own is a validation finding. A call unit
// declares none either: its envelope is synthesized from the child workflow's
// declared `outputs:`, exactly as a call phase's is (§3a).
type Unit struct {
	ID       string `yaml:"id" json:"id"`
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	Model    string `yaml:"model,omitempty" json:"model,omitempty"`
	// Effort pins the reasoning tier of this unit's turn. Like provider/model it
	// is an agent-unit field: a command unit and a call unit run no turn of their
	// own and refuse it.
	Effort  string `yaml:"effort,omitempty" json:"effort,omitempty"`
	Prompt  string `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Command string `yaml:"command,omitempty" json:"command,omitempty"`
	// Resources are the project capacities this unit holds for its own work, on
	// top of the implicit `provider:<name>` slot an agent unit takes. They are
	// unit-scoped and per running unit — the phase's `resources:` are acquired
	// once for the whole attempt — so a wave of units contends on them like any
	// other work. A call unit runs no work of its own and declares none.
	Resources []string            `yaml:"resources,omitempty" json:"resources,omitempty"`
	Call      string              `yaml:"call,omitempty" json:"call,omitempty"`
	Args      map[string]string   `yaml:"args,omitempty" json:"args,omitempty"`
	MaxDepth  int                 `yaml:"max_depth,omitempty" json:"maxDepth,omitempty"`
	Access    Access              `yaml:"access,omitempty" json:"access,omitempty"`
	Outputs   map[string]Variable `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	// AccountsForUnits opts a JOIN into the merge contract: its phase must
	// declare `merged` and `blocked` outputs, and a `done` join envelope is
	// post-validated against the exact set of units the join ran over, so a
	// wave can never be reported complete with a lane silently missing from
	// both lists. It is legal on a join alone — a work unit consolidates
	// nothing. See `join_accounting.go`.
	AccountsForUnits bool `yaml:"accounts_for_units,omitempty" json:"accountsForUnits,omitempty"`
}

// CallTarget is the workflow id a call unit invokes. Like a call phase's it is
// always a static id, which is what lets the dry-run validate the whole call
// graph — unit edges included — before anything runs.
func (u Unit) CallTarget() string { return strings.TrimSpace(u.Call) }

// IsCall reports whether the unit invokes another workflow instead of running
// work of its own. It is the unit-level mirror of Phase.IsCall.
func (u Unit) IsCall() bool { return u.CallTarget() != "" }

// EffectiveShape resolves what a unit is: `call` when it invokes a workflow,
// `single` when it runs one piece of work itself. It reuses the phase-level
// vocabulary for the same distinction rather than inventing a second one, so
// "does this run work of its own" reads identically at both levels.
func (u Unit) EffectiveShape() Shape {
	if u.IsCall() {
		return ShapeCall
	}
	return ShapeSingle
}

// EffectiveDriver resolves how a unit's own work executes. Units never declare
// `driver`: the binding they carry is the discriminator — a `command` runs
// deterministically like a tool phase, a prompt runs an agent turn.
//
// The second result is false for a call unit, which runs no work of its own and
// therefore has no driver: the child workflow's phases carry the drivers. It is
// reported rather than defaulted so a caller cannot silently treat a call unit
// as an agent one and demand a provider it never declares.
func (u Unit) EffectiveDriver() (Driver, bool) {
	if u.IsCall() {
		return "", false
	}
	if strings.TrimSpace(u.Command) != "" {
		return DriverTool, true
	}
	return DriverAgent, true
}

// Gate contains ordered, first-match-wins routes.
type Gate struct {
	Routes []Route `yaml:"routes" json:"routes"`
}

// Route is exactly one forward, loop, park, or human route.
type Route struct {
	When     *Predicate  `yaml:"when,omitempty" json:"when,omitempty"`
	To       string      `yaml:"to,omitempty" json:"to,omitempty"`
	Loop     string      `yaml:"loop,omitempty" json:"loop,omitempty"`
	Max      LoopBound   `yaml:"max,omitempty" json:"max,omitzero"`
	Feedback []string    `yaml:"feedback,omitempty" json:"feedback,omitempty"`
	Park     string      `yaml:"park,omitempty" json:"park,omitempty"`
	Human    *HumanRoute `yaml:"human,omitempty" json:"human,omitempty"`
	// Notify asks for a progress wake when the gate takes this route: the run
	// continues exactly as it would have, and the bound thread additionally
	// hears that it passed here. It is a DECORATION rather than a route kind —
	// a route still declares exactly one of to/loop/park/human — because
	// "tell someone" is orthogonal to where the run goes.
	//
	// It is refused on a `human:` or `park:` route, which already wake the
	// bound thread by parking, and it does nothing on a route to `done` or
	// `failed`, where the run rests and its resting wake is the fuller message
	// (a dry-run Report says so rather than leaving it silently inert).
	Notify bool `yaml:"notify,omitempty" json:"notify,omitempty"`
	// Session decides whether the attempt this loop creates runs on the target
	// phase's OWN previous session or on a new one. It is legal on a `loop:`
	// route alone: every other route kind enters a different phase, which has no
	// session of its own to continue.
	//
	// `fresh` is the default and the behaviour every loop had before this field:
	// cold re-entry, which is what a review phase wants — an adjudicator that
	// remembers arguing for its last verdict is the anchoring the loop exists to
	// break. `continue` is for the FIX edge, where the opposite is true: an
	// implementer re-entered cold has lost what it just tried and re-derives it
	// from the rejection note alone.
	Session RouteSession `yaml:"session,omitempty" json:"session,omitempty"`
	// Prompt overrides the authored prompt body of the phase this loop
	// re-enters, for the one attempt this route creates. It is a
	// sibling-relative file path authored exactly like a phase's `prompt:`, is
	// inlined and frozen with the definition the same way, and is legal on a
	// `loop:` route alone.
	//
	// It exists so "later rounds ask a narrower question" is authorable: a
	// review loop's fix edge can carry a fix-focused prompt, and a convergence
	// loop can carry "only blocking findings extend this loop" without the phase
	// having to branch on its own lap count. It is ROUTE-scoped and never
	// sticky: an attempt that loops again through a route with no `prompt:`
	// renders the phase's own body.
	Prompt string `yaml:"prompt,omitempty" json:"prompt,omitempty"`
}

// RouteSession is the closed vocabulary of a loop route's `session:`.
type RouteSession string

const (
	// SessionFresh creates a new provider session for the re-entered attempt.
	SessionFresh RouteSession = "fresh"
	// SessionContinue runs the re-entered attempt as the next turn of the target
	// phase's most recent session, carrying the loop feedback as its message.
	SessionContinue RouteSession = "continue"
)

// EffectiveSession resolves a route's declared session mode, defaulting to
// SessionFresh. Every consumer goes through it, so an unset field and an
// explicit `fresh` can never be read differently — and a frozen snapshot that
// predates the field resolves to the behaviour it actually ran under.
func (r Route) EffectiveSession() RouteSession {
	if r.Session == SessionContinue {
		return SessionContinue
	}
	return SessionFresh
}

type HumanRoute struct {
	Approve string      `yaml:"approve" json:"approve"`
	Reject  *LoopTarget `yaml:"reject" json:"reject"`
}

type LoopTarget struct {
	Loop     string    `yaml:"loop" json:"loop"`
	Max      LoopBound `yaml:"max" json:"max"`
	Feedback []string  `yaml:"feedback,omitempty" json:"feedback,omitempty"`
}

// Predicate is a closed recursive predicate language. Exactly one field is set.
type Predicate struct {
	Eq     *Comparison `yaml:"eq,omitempty" json:"eq,omitempty"`
	Neq    *Comparison `yaml:"neq,omitempty" json:"neq,omitempty"`
	Gt     *Comparison `yaml:"gt,omitempty" json:"gt,omitempty"`
	Gte    *Comparison `yaml:"gte,omitempty" json:"gte,omitempty"`
	Lt     *Comparison `yaml:"lt,omitempty" json:"lt,omitempty"`
	Lte    *Comparison `yaml:"lte,omitempty" json:"lte,omitempty"`
	In     *Membership `yaml:"in,omitempty" json:"in,omitempty"`
	Exists string      `yaml:"exists,omitempty" json:"exists,omitempty"`
	All    []Predicate `yaml:"all,omitempty" json:"all,omitempty"`
	Any    []Predicate `yaml:"any,omitempty" json:"any,omitempty"`
	Not    *Predicate  `yaml:"not,omitempty" json:"not,omitempty"`
}

type Comparison struct {
	Ref   string `yaml:"ref" json:"ref"`
	Value any    `yaml:"value" json:"value"`
}

type Membership struct {
	Ref    string `yaml:"ref" json:"ref"`
	Values []any  `yaml:"values" json:"values"`
}

// ResolvedWorkflow carries definition provenance used by prompt validation.
type ResolvedWorkflow struct {
	Workflow       Workflow `json:"workflow"`
	Scope          Scope    `json:"scope"`
	Path           string   `json:"path"`
	HumanGateCount int      `json:"humanGateCount"`
}

// CountHumanGates returns the number of phases that can route through an
// explicit human approval gate. A phase counts once even when more than one
// predicate can reach a human route.
func CountHumanGates(workflow Workflow) int {
	count := 0
	for _, phase := range workflow.Phases {
		for _, route := range phase.Gate.Routes {
			if route.Human == nil {
				continue
			}
			count++
			break
		}
	}
	return count
}

// Bindings is the narrow profile-facing surface used by dry-run validation.
// Capacity returns the declared capacity and whether the resource is bound at
// all, so one method answers both "is this bindable" and "how wide can this
// fan-out actually run".
type Bindings interface {
	HasCheck(name string) bool
	Capacity(name string) (int, bool)
	HasCommand(name string) bool
	// DeclaredMaxFanOutWidth is the project's absolute fan-out ceiling, or 0
	// when it declares none. Unlike Capacity there is no "is it bound" half:
	// the ceiling always exists, and an undeclared one resolves to
	// DefaultMaxFanOutWidth through EffectiveMaxFanOutWidth.
	DeclaredMaxFanOutWidth() int
}

type BindingStatus string

const (
	BindingsChecked   BindingStatus = "checked"
	BindingsUnchecked BindingStatus = "unchecked"
)

// Finding is one stable, user-facing dry-run diagnostic.
type Finding struct {
	Code    string `json:"code"`
	Element string `json:"element"`
	Message string `json:"message"`
}

func (f Finding) Error() string { return fmt.Sprintf("%s: %s", f.Element, f.Message) }

// ValidationResult reports every independently discoverable error, plus the
// dry-run's informational reports. Reports never make a workflow invalid: they
// describe how a valid definition will behave at run time (a fan-out wider than
// its provider capacity throttles rather than failing).
type ValidationResult struct {
	Findings      []Finding     `json:"findings"`
	Reports       []Finding     `json:"reports,omitempty"`
	BindingStatus BindingStatus `json:"bindingStatus"`
}

func (r ValidationResult) Valid() bool { return len(r.Findings) == 0 }

// DefaultAccess is what an omitted `access:` declaration means. Read-only is
// the safe default in both directions it governs: an unannotated phase neither
// provisions a worktree nor gets a writable provider session, so forgetting
// the field can only ever under-privilege a phase, never let an unattended
// agent loose on the project root.
const DefaultAccess = AccessReadOnly

// EffectiveAccess resolves the phase's declared access, defaulting to
// DefaultAccess when unset. Every consumer — workspace derivation and the
// provider session's runtime mode — goes through this one predicate so the
// two can never disagree about what a phase is allowed to do.
func (p Phase) EffectiveAccess() Access { return normalizeAccess(p.Access) }

// EffectiveAccess resolves the unit's declared access, defaulting to
// DefaultAccess when unset. Fan-out units and joins carry their own access
// independent of the owning phase.
func (u Unit) EffectiveAccess() Access { return normalizeAccess(u.Access) }

// normalizeAccess treats any value other than an explicit `write` as
// read-only. An unrecognised string reaching here is already a validation
// finding; resolving it to the restrictive side means a typo cannot widen a
// phase's privileges while the author waits for the error.
func normalizeAccess(access Access) Access {
	if access == AccessWrite {
		return AccessWrite
	}
	return DefaultAccess
}

// Writes reports whether the phase itself, its join, or any of its fan-out
// units needs write access. This is the per-phase half of DeriveWorkspaceNeed.
// A dynamic fan-out's single template counts once: its stamped units share the
// template's access, so one writing template means the run needs a worktree
// however many elements the array carries at run time.
func (p Phase) Writes() bool {
	if p.EffectiveAccess() == AccessWrite {
		return true
	}
	if p.Join != nil && p.Join.EffectiveAccess() == AccessWrite {
		return true
	}
	for _, unit := range p.UnitDefinitions() {
		if unit.EffectiveAccess() == AccessWrite {
			return true
		}
	}
	return false
}

// WorkspaceNeed is derived from phase write capabilities, never authored.
type WorkspaceNeed string

const (
	WorkspaceProjectRoot WorkspaceNeed = "project-root-read-only"
	WorkspaceWorktree    WorkspaceNeed = "worktree"
)

// DeriveWorkspaceNeed returns a worktree iff any phase or fan-out unit writes.
// It shares Phase.Writes — and therefore EffectiveAccess — with the session
// runtime-mode mapping, so a workflow can never be given a worktree it is not
// allowed to write to, or write access to a workspace it never provisioned.
func DeriveWorkspaceNeed(workflow Workflow) WorkspaceNeed {
	for _, phase := range workflow.Phases {
		if phase.Writes() {
			return WorkspaceWorktree
		}
	}
	return WorkspaceProjectRoot
}
