package settings

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// The agent roster: the profiles an agent spawns a sub-thread by, the notes
// on choosing between them, and how many levels of sub-threads may exist
// under a thread the person talks to (docs/specs/agent-thread-tools.md,
// "Agent profiles" and "Sub-thread depth"). internal/threadtools reads them
// through the app and resolves each profile against the catalog of the
// computer a spawn targets; nothing here knows what a computer offers.
//
// Provider ids, efforts and runtime modes are restated here, as the
// text-generation efforts are, because this package stays free of
// internal/provider; internal/app's tests pin them to the provider sets.

// AgentProfile is one agent the person set up for spawning. Every setting
// but Name and UseFor is optional: empty inherits the spawner's.
type AgentProfile struct {
	// Name is what a spawn names the profile by, unique without regard to
	// case.
	Name string `json:"name"`
	// UseFor is the line an agent reads when it chooses: what the profile
	// is good at, what it costs, and what not to use it for.
	UseFor   string `json:"useFor"`
	Provider string `json:"provider"`
	// Model is an exact model ID, or a family named without a version
	// (`claude-opus`, `gpt-sol`) that runs the newest model of it the
	// target computer lists. The provider's grammar decides which it is
	// (internal/provider/modelfamily.go), so nothing here reads it.
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	RuntimeMode string `json:"runtimeMode"`
	// Instructions are appended to the system prompt of every sub-thread
	// spawned with the profile, after what the app and the person already
	// append, never replacing it.
	Instructions string `json:"instructions"`
}

// AgentNote is one note on choosing an agent. When decides whether an
// agent is shown it, from the profiles it could spawn on a computer.
type AgentNote struct {
	Text string `json:"text"`
	When string `json:"when"`
}

// When values of an AgentNote.
const (
	// AgentNoteAlways shows the note whenever any profile is listed.
	AgentNoteAlways = ""
	// AgentNoteBothProviders shows the note when the listed profiles run
	// on both Claude and Codex.
	AgentNoteBothProviders = "both_providers"
	// AgentNoteOneProvider shows the note when the listed profiles all run
	// on one provider and on at least two different models.
	AgentNoteOneProvider = "one_provider"
)

// Bounds for the roster. Every thread_options answer and every guide lists
// it, so a hand-edited file must not be able to make either unbounded.
const (
	MaxAgentProfiles                 = 24
	MaxAgentProfileNameRunes         = 32
	MaxAgentProfileUseForRunes       = 400
	MaxAgentProfileModelRunes        = 128
	MaxAgentProfileInstructionsBytes = 4096
	MaxAgentNotes                    = 8
	MaxAgentNoteRunes                = 800
	MinSubThreadLevels               = 1
	MaxSubThreadLevels               = 4
	DefaultSubThreadLevels           = 2
)

var (
	agentProfileProviders = map[string]struct{}{"": {}, "claude": {}, "codex": {}}
	agentProfileEfforts   = map[string]struct{}{
		"": {}, "none": {}, "minimal": {}, "low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {}, "ultra": {},
	}
	agentProfileRuntimeModes = map[string]struct{}{
		"": {}, "read-only": {}, "approval-required": {}, "auto-accept-edits": {}, "auto": {}, "full-access": {},
	}
	agentNoteWhens = map[string]struct{}{
		AgentNoteAlways: {}, AgentNoteBothProviders: {}, AgentNoteOneProvider: {},
	}
	// agentProfileName is a word or a few: letters, digits, spaces, dots,
	// dashes and underscores, starting with a letter or digit.
	agentProfileName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]*$`)
)

// AgentProfileProviders, AgentProfileEfforts and AgentProfileRuntimeModes
// list the non-empty values a profile may name, for the tests that pin them
// to internal/provider.
func AgentProfileProviders() []string    { return sortedNonEmpty(agentProfileProviders) }
func AgentProfileEfforts() []string      { return sortedNonEmpty(agentProfileEfforts) }
func AgentProfileRuntimeModes() []string { return sortedNonEmpty(agentProfileRuntimeModes) }

func sortedNonEmpty(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// The seeded roster: one profile per model family the owner described, in
// the owner's words made neutral, grouped by provider and ordered within
// each from the lowest tier to the highest, so the listing shows the
// ladder. Each names its family, so it runs the newest model of it a
// computer lists. A use-for line names only profiles of its own provider: a
// computer with one provider must never be pointed at the other's. A
// computer that lists no model of a profile's family leaves the profile
// out of thread_options there.
var defaultAgentProfiles = []AgentProfile{
	{
		Name: "Luna", Provider: "codex", Model: "gpt-luna", Effort: "high",
		UseFor: "Mechanical changes that sweep across many files, fanned out to many threads, with little or no judgment; or a search for something specific.",
	},
	{
		Name: "Terra", Provider: "codex", Model: "gpt-terra", Effort: "high",
		UseFor: "A clear task where speed and cost matter and no problems are expected. Tell it to ask you before any major decision. The best value for its cost.",
	},
	{
		Name: "Sol", Provider: "codex", Model: "gpt-sol", Effort: "high",
		UseFor: "Code review, including adversarial review: detail-oriented and thorough. Faster and cheaper than Astra and the better reviewer of the two.",
	},
	{
		Name: "Astra", Provider: "codex", Model: "gpt-astra", Effort: "high",
		UseFor: "Architecture and big-picture design discussion; the strongest reasoner. Also a careful, detail-oriented reviewer, though Sol reviews as well for less. Expensive and slow. Do not use it to implement or to make the final call: it is unreliable at both.",
	},
	{
		Name: "Haiku", Provider: "claude", Model: "claude-haiku", Effort: "high",
		UseFor: "Mechanical changes that sweep across many files, fanned out to many threads, with little or no judgment; or a search for something specific.",
	},
	{
		Name: "Sonnet", Provider: "claude", Model: "claude-sonnet", Effort: "high",
		UseFor: "A clear task where speed and cost matter and no problems are expected. Tell it to ask you before any major decision.",
	},
	{
		Name: "Opus", Provider: "claude", Model: "claude-opus", Effort: "high",
		UseFor: "Implementation: the dependable workhorse when the work needs decisions about a clean design.",
	},
	{
		Name: "Fable", Provider: "claude", Model: "claude-fable", Effort: "high",
		UseFor: "Design and product taste, and common-sense judgment on a plan or a result. Very expensive; moderate speed. Not for routine implementation.",
	},
}

// The seeded notes. Advice that needs both families is its own note with
// its condition, beside the same advice phrased for one family, so an
// agent is never told to draw on a provider it cannot run and the person
// can still edit each text. Only such a note compares profiles across the
// families.
var defaultAgentNotes = []AgentNote{
	{
		When: AgentNoteAlways,
		Text: "Tier and effort are two separate choices. Tier is capability: a higher-tier model thinks more deeply and knows more, and each provider's profiles are listed from the lowest tier to the highest. Effort is diligence: how much careful work the model puts into the task. Choose the tier by how hard the thinking is and the effort by how much careful work there is. A lower tier at xhigh can occasionally beat a higher tier at low or medium when the task is mostly diligence, though rarely.",
	},
	{
		When: AgentNoteAlways,
		Text: "Effort: use high by default. Use xhigh only for a genuinely hard problem, and more for reviews and architecture than for implementation, where it tends to cause overthinking. A task that needs taste but little judgment suits a stronger model at low or medium effort; a purely mechanical task suits a cheaper model at high effort.",
	},
	{
		When: AgentNoteBothProviders,
		Text: "Across the two families: Terra is probably the best value, and Sonnet is about the same; Haiku and Luna are about the same.",
	},
	{
		When: AgentNoteBothProviders,
		Text: "For a big judgment call, such as a complex decision, plan or design, or an important review, convene a council: about three models drawn from both the Claude and the Codex profiles, each asked the same question by one lead thread that compares the answers and reconciles them.",
	},
	{
		When: AgentNoteOneProvider,
		Text: "For a big judgment call, such as a complex decision, plan or design, or an important review, convene a council: about three different models, each asked the same question by one lead thread that compares the answers and reconciles them.",
	},
	{
		When: AgentNoteAlways,
		Text: "Sub-threads have the same thread tools as you. When sub-threads should work together, give each the others' thread ids and they can message each other directly.",
	},
}

// validateAgentRoster is the strict path: entries are trimmed, and an entry
// the roster could not use is refused, naming it.
func validateAgentRoster(current *Settings) error {
	if current.SubThreadLevels < MinSubThreadLevels || current.SubThreadLevels > MaxSubThreadLevels {
		return fmt.Errorf("subThreadLevels must be between %d and %d", MinSubThreadLevels, MaxSubThreadLevels)
	}
	if len(current.AgentProfiles) > MaxAgentProfiles {
		return fmt.Errorf("agentProfiles has %d profiles; at most %d are allowed", len(current.AgentProfiles), MaxAgentProfiles)
	}
	profiles := make([]AgentProfile, 0, len(current.AgentProfiles))
	seen := make(map[string]bool, len(current.AgentProfiles))
	for index, profile := range current.AgentProfiles {
		profile = trimAgentProfile(profile)
		if problem := agentProfileProblem(profile); problem != "" {
			return fmt.Errorf("agentProfiles[%d] %s", index, problem)
		}
		key := strings.ToLower(profile.Name)
		if seen[key] {
			return fmt.Errorf("agentProfiles has two profiles named %q; names must differ", profile.Name)
		}
		seen[key] = true
		profiles = append(profiles, profile)
	}
	current.AgentProfiles = profiles
	if len(current.AgentNotes) > MaxAgentNotes {
		return fmt.Errorf("agentNotes has %d notes; at most %d are allowed", len(current.AgentNotes), MaxAgentNotes)
	}
	notes := make([]AgentNote, 0, len(current.AgentNotes))
	for index, note := range current.AgentNotes {
		note = AgentNote{Text: strings.TrimSpace(note.Text), When: strings.TrimSpace(note.When)}
		if problem := agentNoteProblem(note); problem != "" {
			return fmt.Errorf("agentNotes[%d] %s", index, problem)
		}
		notes = append(notes, note)
	}
	current.AgentNotes = notes
	return nil
}

// sanitizeAgentRoster is the lenient load path: whatever validation would
// refuse is dropped, and logged.
func sanitizeAgentRoster(current *Settings) {
	if current.SubThreadLevels < MinSubThreadLevels || current.SubThreadLevels > MaxSubThreadLevels {
		log.Printf("settings: subThreadLevels %d outside %d to %d, using %d", current.SubThreadLevels, MinSubThreadLevels, MaxSubThreadLevels, DefaultSubThreadLevels)
		current.SubThreadLevels = DefaultSubThreadLevels
	}
	// Both paths leave arrays, never nil: a client reads null as "no
	// value" and would show the seeds in place of an emptied roster.
	{
		profiles := make([]AgentProfile, 0, len(current.AgentProfiles))
		seen := make(map[string]bool, len(current.AgentProfiles))
		for index, profile := range current.AgentProfiles {
			profile = trimAgentProfile(profile)
			problem := agentProfileProblem(profile)
			if problem == "" && seen[strings.ToLower(profile.Name)] {
				problem = "repeats a name"
			}
			if problem == "" && len(profiles) == MaxAgentProfiles {
				problem = fmt.Sprintf("is past the first %d", MaxAgentProfiles)
			}
			if problem != "" {
				log.Printf("settings: dropping agentProfiles[%d] %q: %s", index, profile.Name, problem)
				continue
			}
			seen[strings.ToLower(profile.Name)] = true
			profiles = append(profiles, profile)
		}
		current.AgentProfiles = profiles
	}
	{
		notes := make([]AgentNote, 0, len(current.AgentNotes))
		for index, note := range current.AgentNotes {
			note = AgentNote{Text: strings.TrimSpace(note.Text), When: strings.TrimSpace(note.When)}
			problem := agentNoteProblem(note)
			if problem == "" && len(notes) == MaxAgentNotes {
				problem = fmt.Sprintf("is past the first %d", MaxAgentNotes)
			}
			if problem != "" {
				log.Printf("settings: dropping agentNotes[%d]: %s", index, problem)
				continue
			}
			notes = append(notes, note)
		}
		current.AgentNotes = notes
	}
}

func trimAgentProfile(profile AgentProfile) AgentProfile {
	return AgentProfile{
		Name:         strings.TrimSpace(profile.Name),
		UseFor:       strings.TrimSpace(profile.UseFor),
		Provider:     strings.TrimSpace(profile.Provider),
		Model:        strings.TrimSpace(profile.Model),
		Effort:       strings.TrimSpace(profile.Effort),
		RuntimeMode:  strings.TrimSpace(profile.RuntimeMode),
		Instructions: strings.TrimSpace(profile.Instructions),
	}
}

// agentProfileProblem says what is wrong with one trimmed profile, or "".
func agentProfileProblem(profile AgentProfile) string {
	switch {
	case profile.Name == "":
		return "needs a name"
	case utf8.RuneCountInString(profile.Name) > MaxAgentProfileNameRunes:
		return fmt.Sprintf("name %q must be at most %d characters", profile.Name, MaxAgentProfileNameRunes)
	case !agentProfileName.MatchString(profile.Name):
		return fmt.Sprintf("name %q may use only letters, digits, spaces, dots, dashes and underscores", profile.Name)
	case profile.UseFor == "":
		return fmt.Sprintf("%q needs a use-for line: agents choose a profile by it", profile.Name)
	case utf8.RuneCountInString(profile.UseFor) > MaxAgentProfileUseForRunes:
		return fmt.Sprintf("%q use-for line must be at most %d characters", profile.Name, MaxAgentProfileUseForRunes)
	case !inSet(agentProfileProviders, profile.Provider):
		return fmt.Sprintf("%q provider must be one of %s, or empty to inherit", profile.Name, strings.Join(AgentProfileProviders(), ", "))
	case utf8.RuneCountInString(profile.Model) > MaxAgentProfileModelRunes:
		return fmt.Sprintf("%q model must be at most %d characters", profile.Name, MaxAgentProfileModelRunes)
	case profile.Model != "" && profile.Provider == "":
		return fmt.Sprintf("%q names a model, so it needs the provider that runs it", profile.Name)
	case !inSet(agentProfileEfforts, profile.Effort):
		return fmt.Sprintf("%q effort must be one of %s, or empty to inherit", profile.Name, strings.Join(AgentProfileEfforts(), ", "))
	case !inSet(agentProfileRuntimeModes, profile.RuntimeMode):
		return fmt.Sprintf("%q runtime mode must be one of %s, or empty to inherit", profile.Name, strings.Join(AgentProfileRuntimeModes(), ", "))
	case len(profile.Instructions) > MaxAgentProfileInstructionsBytes:
		return fmt.Sprintf("%q instructions must be at most %d bytes", profile.Name, MaxAgentProfileInstructionsBytes)
	}
	return ""
}

func agentNoteProblem(note AgentNote) string {
	switch {
	case note.Text == "":
		return "needs text"
	case utf8.RuneCountInString(note.Text) > MaxAgentNoteRunes:
		return fmt.Sprintf("must be at most %d characters", MaxAgentNoteRunes)
	case !inSet(agentNoteWhens, note.When):
		return fmt.Sprintf("when must be %s or %s, or empty for always", AgentNoteBothProviders, AgentNoteOneProvider)
	}
	return ""
}

func inSet(set map[string]struct{}, value string) bool {
	_, ok := set[value]
	return ok
}
