package profile

import (
	"fmt"
	"strings"

	"agent-overflow/internal/workflow/def"
)

// Duration is an authored time.ParseDuration-compatible value.
type Duration string

// Disposition controls what happens to a successfully completed worktree.
type Disposition string

const (
	DispositionManual    Disposition = "manual"
	DispositionAutoPR    Disposition = "auto-pr"
	DispositionAutoMerge Disposition = "auto-merge"
)

// Profile is the complete per-project workflow profile authoring format.
type Profile struct {
	BaseBranch string              `yaml:"base_branch,omitempty" json:"baseBranch,omitempty"`
	Checks     map[string][]string `yaml:"checks,omitempty" json:"checks,omitempty"`
	Capacities map[string]int      `yaml:"capacities,omitempty" json:"capacities,omitempty"`
	// MaxFanOutWidth is the project's absolute ceiling on the units one fan-out
	// phase attempt may expand to. It is not a capacity: a capacity throttles
	// work that will still all run, while this refuses the attempt outright.
	// It lives on the project rather than on the workflow precisely because a
	// definition that could raise its own ceiling would not be a ceiling —
	// the number is a fact about what this project's provider subscriptions can
	// absorb, and only the project gets to state it.
	//
	// It is a pointer for the same reason a Budget's fields are: absent has to
	// be distinguishable from an authored zero. Absent means
	// def.DefaultMaxFanOutWidth; an authored zero or negative is a finding,
	// because a zero ceiling would forbid every fan-out and nobody means that.
	// There is deliberately no value that means unlimited.
	MaxFanOutWidth *int                `yaml:"max_fan_out_width,omitempty" json:"maxFanOutWidth,omitempty"`
	Commands       map[string][]string `yaml:"commands,omitempty" json:"commands,omitempty"`
	Reliability    ReliabilityDefaults `yaml:"reliability,omitempty" json:"reliability,omitempty"`
	Disposition    Disposition         `yaml:"disposition,omitempty" json:"disposition"`
	Secrets        map[string]Secret   `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	MCPServers     []string            `yaml:"mcp_servers,omitempty" json:"mcpServers,omitempty"`
	WorktreeSetup  WorktreeSetup       `yaml:"worktree_setup,omitempty" json:"worktreeSetup,omitempty"`
}

// ReliabilityDefaults supplies project-level watchdog and runaway ceilings.
type ReliabilityDefaults struct {
	Watchdog      Duration   `yaml:"watchdog,omitempty" json:"watchdog,omitempty"`
	Backoff       []Duration `yaml:"backoff,omitempty" json:"backoff,omitempty"`
	PerItemBudget *Budget    `yaml:"per_item_budget,omitempty" json:"perItemBudget,omitempty"`
}

// Budget is one optional per-item ceiling. Exactly one field must be set.
type Budget struct {
	Tokens    *int64    `yaml:"tokens,omitempty" json:"tokens,omitempty"`
	USD       *float64  `yaml:"usd,omitempty" json:"usd,omitempty"`
	WallClock *Duration `yaml:"wall_clock,omitempty" json:"wallClock,omitempty"`
}

// Secret names one host-side source. Values are resolved only on request.
type Secret struct {
	Source string `yaml:"source" json:"source"`
	Env    string `yaml:"env,omitempty" json:"env,omitempty"`
	Path   string `yaml:"path,omitempty" json:"path,omitempty"`
}

// WorktreeSetup describes files to copy and argv commands to run before phases.
// This package validates the declaration but never executes it.
type WorktreeSetup struct {
	Copy    []string   `yaml:"copy,omitempty" json:"copy,omitempty"`
	Run     [][]string `yaml:"run,omitempty" json:"run,omitempty"`
	Timeout Duration   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Default returns the documented profile used when profile.yaml is absent. It
// leaves MaxFanOutWidth unset rather than stamping the default in: "unset means
// def.DefaultMaxFanOutWidth" then has exactly one implementation
// (def.EffectiveMaxFanOutWidth) instead of two that could drift, and a project
// with no profile at all is bounded by the same code path as one whose profile
// simply omits the key.
func Default() Profile {
	return Profile{
		Disposition:   DispositionManual,
		Reliability:   ReliabilityDefaults{Backoff: DefaultBackoff()},
		WorktreeSetup: WorktreeSetup{Timeout: DefaultWorktreeSetupTimeout},
	}
}

const (
	DefaultWorktreeSetupTimeout = "10m"
	DefaultBackoffFirst         = "30s"
	DefaultBackoffSecond        = "2m"
	DefaultBackoffThird         = "5m"
)

// DefaultBackoff returns an isolated copy of the documented transient retry
// schedule. Callers may safely retain or modify the returned slice.
func DefaultBackoff() []Duration {
	return []Duration{DefaultBackoffFirst, DefaultBackoffSecond, DefaultBackoffThird}
}

// HasCheck reports whether a named deterministic check is bound.
func (p *Profile) HasCheck(name string) bool {
	if p == nil {
		return false
	}
	_, ok := p.Checks[name]
	return ok
}

// Capacity returns a named project-local resource's declared capacity and
// whether the profile binds it at all.
func (p *Profile) Capacity(name string) (int, bool) {
	if p == nil {
		return 0, false
	}
	capacity, ok := p.Capacities[name]
	return capacity, ok
}

// HasCommand reports whether a named command template is bound.
func (p *Profile) HasCommand(name string) bool {
	if p == nil {
		return false
	}
	_, ok := p.Commands[name]
	return ok
}

// DeclaredMaxFanOutWidth returns the project's own fan-out ceiling, or 0 when
// it declares none. Validate refuses a declared value below 1, so a 0 from here
// only ever means "undeclared"; def.EffectiveMaxFanOutWidth is the one place
// that resolves it to the default, so the dry-run and the engine cannot
// disagree about the number.
func (p *Profile) DeclaredMaxFanOutWidth() int {
	if p == nil || p.MaxFanOutWidth == nil {
		return 0
	}
	return *p.MaxFanOutWidth
}

var _ def.Bindings = (*Profile)(nil)

// Finding is one stable profile validation diagnostic.
type Finding struct {
	Code    string `json:"code"`
	Element string `json:"element"`
	Message string `json:"message"`
}

func (f Finding) Error() string { return fmt.Sprintf("%s: %s", f.Element, f.Message) }

// ValidationResult reports every independently discoverable profile error.
type ValidationResult struct {
	Findings []Finding `json:"findings"`
}

func (r ValidationResult) Valid() bool { return len(r.Findings) == 0 }

// ValidationError is returned when a parsed profile is structurally invalid.
type ValidationError struct {
	Findings []Finding
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Findings))
	for _, finding := range e.Findings {
		parts = append(parts, finding.Error())
	}
	return strings.Join(parts, "; ")
}
