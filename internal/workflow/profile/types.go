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
	BaseBranch    string              `yaml:"base_branch,omitempty" json:"baseBranch,omitempty"`
	Checks        map[string][]string `yaml:"checks,omitempty" json:"checks,omitempty"`
	Capacities    map[string]int      `yaml:"capacities,omitempty" json:"capacities,omitempty"`
	Commands      map[string][]string `yaml:"commands,omitempty" json:"commands,omitempty"`
	Reliability   ReliabilityDefaults `yaml:"reliability,omitempty" json:"reliability,omitempty"`
	Disposition   Disposition         `yaml:"disposition,omitempty" json:"disposition"`
	Secrets       map[string]Secret   `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	MCPServers    []string            `yaml:"mcp_servers,omitempty" json:"mcpServers,omitempty"`
	WorktreeSetup WorktreeSetup       `yaml:"worktree_setup,omitempty" json:"worktreeSetup,omitempty"`
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
	Copy []string   `yaml:"copy,omitempty" json:"copy,omitempty"`
	Run  [][]string `yaml:"run,omitempty" json:"run,omitempty"`
}

// Default returns the documented profile used when profile.yaml is absent.
func Default() Profile {
	return Profile{
		Disposition: DispositionManual,
		Reliability: ReliabilityDefaults{Backoff: DefaultBackoff()},
	}
}

const (
	DefaultBackoffFirst  = "30s"
	DefaultBackoffSecond = "2m"
	DefaultBackoffThird  = "5m"
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

// HasCapacity reports whether a named project-local resource is bound.
func (p *Profile) HasCapacity(name string) bool {
	if p == nil {
		return false
	}
	_, ok := p.Capacities[name]
	return ok
}

// HasCommand reports whether a named command template is bound.
func (p *Profile) HasCommand(name string) bool {
	if p == nil {
		return false
	}
	_, ok := p.Commands[name]
	return ok
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
