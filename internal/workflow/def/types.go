package def

import "fmt"

// Scope identifies where a workflow definition was loaded from.
type Scope string

const (
	ScopeShared  Scope = "shared"
	ScopeProject Scope = "project"
)

// Workflow is the complete portable workflow authoring format.
type Workflow struct {
	ID              string                    `yaml:"id" json:"id"`
	Name            string                    `yaml:"name" json:"name"`
	Description     string                    `yaml:"description,omitempty" json:"description,omitempty"`
	Inputs          map[string]Variable       `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Outputs         map[string]WorkflowOutput `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	Phases          []Phase                   `yaml:"phases" json:"phases"`
	DefaultStepMode bool                      `yaml:"default_step_mode,omitempty" json:"defaultStepMode,omitempty"`
	Cleanup         CleanupPolicy             `yaml:"cleanup,omitempty" json:"cleanup,omitempty"`
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
}

// WorkflowOutput names a run deliverable sourced from a phase output.
type WorkflowOutput struct {
	From     string `yaml:"from" json:"from"`
	Artifact bool   `yaml:"artifact,omitempty" json:"artifact,omitempty"`
}

// Phase is one graph node and its ordered exit routes.
type Phase struct {
	ID           string              `yaml:"id" json:"id"`
	Name         string              `yaml:"name,omitempty" json:"name,omitempty"`
	Driver       Driver              `yaml:"driver" json:"driver"`
	Shape        Shape               `yaml:"shape,omitempty" json:"shape,omitempty"`
	Provider     string              `yaml:"provider,omitempty" json:"provider,omitempty"`
	Model        string              `yaml:"model,omitempty" json:"model,omitempty"`
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
	Access       Access              `yaml:"access,omitempty" json:"access,omitempty"`
	FanOut       []Unit              `yaml:"fan_out,omitempty" json:"fanOut,omitempty"`
	Join         *Unit               `yaml:"join,omitempty" json:"join,omitempty"`
	Gate         Gate                `yaml:"gate" json:"gate"`
}

type Driver string
type Shape string
type Access string

const (
	DriverAgent    Driver = "agent"
	DriverTool     Driver = "tool"
	ShapeSingle    Shape  = "single"
	ShapeFanOut    Shape  = "fan-out"
	AccessReadOnly Access = "read-only"
	AccessWrite    Access = "write"
)

// Unit configures one fan-out worker or its required join.
type Unit struct {
	ID       string `yaml:"id" json:"id"`
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	Model    string `yaml:"model,omitempty" json:"model,omitempty"`
	Prompt   string `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Command  string `yaml:"command,omitempty" json:"command,omitempty"`
	Access   Access `yaml:"access,omitempty" json:"access,omitempty"`
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
	Max      int         `yaml:"max,omitempty" json:"max,omitempty"`
	Feedback []string    `yaml:"feedback,omitempty" json:"feedback,omitempty"`
	Park     string      `yaml:"park,omitempty" json:"park,omitempty"`
	Human    *HumanRoute `yaml:"human,omitempty" json:"human,omitempty"`
}

type HumanRoute struct {
	Approve string      `yaml:"approve" json:"approve"`
	Reject  *LoopTarget `yaml:"reject" json:"reject"`
}

type LoopTarget struct {
	Loop     string   `yaml:"loop" json:"loop"`
	Max      int      `yaml:"max" json:"max"`
	Feedback []string `yaml:"feedback,omitempty" json:"feedback,omitempty"`
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
type Bindings interface {
	HasCheck(name string) bool
	HasCapacity(name string) bool
	HasCommand(name string) bool
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

// ValidationResult reports every independently discoverable error.
type ValidationResult struct {
	Findings      []Finding     `json:"findings"`
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
func (p Phase) Writes() bool {
	if p.EffectiveAccess() == AccessWrite {
		return true
	}
	if p.Join != nil && p.Join.EffectiveAccess() == AccessWrite {
		return true
	}
	for _, unit := range p.FanOut {
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
