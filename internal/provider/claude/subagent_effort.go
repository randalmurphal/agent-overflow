package claude

import (
	"encoding/json"
	"regexp"
	"strings"

	"agent-overflow/internal/provider"
)

// Subagent reasoning effort (claude-wire.md §"Agent effort").
//
// The Agent tool takes an optional `effort` input (Claude Code 2.1.292+).
// The CLI reports no effective effort for a subagent on the stream, so
// what AO records is what was true when the agent started running: the
// input the launch carries, and `inherited_effort`, the effort the launching
// agent ran at. A launch that names no effort, and every fork, runs at its
// launcher's effort: the session's for a top-level launch, the enclosing
// subagent's for a nested one. A §E6 resume runs at the launch's named
// effort, else at its sender's effort at resume time, so the parser stamps
// `inherited_effort` on the resume carrier too. Each stamp is written once
// on its row because the session's effort can change afterwards (`/effort`).
//
// The launch's requested model alias is resolved here as well
// (`requested_model`), because the alias's target decides which effort the
// model can run at before the subagent reports its model.

const (
	// inheritedEffortMetaKey is the launch- and carrier-row meta key of the
	// launcher's effort.
	inheritedEffortMetaKey = "inherited_effort"
	// requestedModelMetaKey is the launch-row meta key of the model the
	// launch's `model` alias resolves to.
	requestedModelMetaKey = "requested_model"
	// RequestEffortKey is the claude-tui reconstructor's top-level field on
	// a main-loop assistant envelope: the `output_config.effort` of the API
	// request that produced it, "" when the request named none.
	RequestEffortKey = "ao_request_effort"
)

// forkSubagentType is the subagent_type whose input effort and model the
// CLI ignores.
const forkSubagentType = "fork"

// SetSessionEffort seeds the effort the session was spawned with: the
// resolved `--effort` value, empty when none was passed (the CLI then
// picks its own, which AO does not know).
func (p *Parser) SetSessionEffort(effort string) {
	if p == nil {
		return
	}
	p.sessionEffort = strings.TrimSpace(effort)
}

// SetModelAliases gives the parser the CLI's alias map (ModelAliases), which
// resolves an Agent launch's `model` alias.
func (p *Parser) SetModelAliases(aliases map[string]string) {
	if p == nil {
		return
	}
	p.modelAliases = aliases
}

// effortCapReplyPattern matches the /effort reply that sets a tier the user
// did not ask for: "Effort 'max' exceeds the cap for <model> set by your
// settings or organization; set to 'high' instead…" (2.1.300).
var effortCapReplyPattern = regexp.MustCompile(`^Effort '[^']*' exceeds the cap for .*?; set to '([^']+)' instead`)

// effortClearedReplyPrefixes are the /effort replies after which the
// session runs at an effort AO cannot name: `/effort auto`, with or
// without CLAUDE_CODE_EFFORT_LEVEL overriding it (2.1.300).
var effortClearedReplyPrefixes = []string{
	"Effort level set to auto",
	"Effort set to auto for this session, but",
	"Cleared effort from settings, but",
}

// observeEffortReply follows a `/effort` reply. A tier AO cannot represent
// ("auto", "ultracode") leaves the session's effort unknown.
func (p *Parser) observeEffortReply(text string) {
	if strings.HasPrefix(text, effortSetReplyPrefix) {
		tier, _ := EffortSetReplyTier(text)
		p.sessionEffort = tier
		return
	}
	if match := effortCapReplyPattern.FindStringSubmatch(text); match != nil {
		p.sessionEffort = ""
		if IsLiveEffortTier(match[1]) {
			p.sessionEffort = match[1]
		}
		return
	}
	for _, prefix := range effortClearedReplyPrefixes {
		if strings.HasPrefix(text, prefix) {
			p.sessionEffort = ""
			return
		}
	}
}

// observeRequestEffort follows the effort of the main-loop API request a
// claude-tui assistant envelope came from. The terminal's `/effort` never
// reaches the parser, but every main-loop request names the effort it runs
// at, so this is the TUI's record of the session's effort.
func (p *Parser) observeRequestEffort(raw map[string]json.RawMessage, parentToolUseID string) {
	value, ok := raw[RequestEffortKey]
	if !ok || parentToolUseID != "" {
		return
	}
	var effort string
	_ = json.Unmarshal(value, &effort)
	effort = strings.TrimSpace(effort)
	if !IsLiveEffortTier(effort) {
		effort = ""
	}
	p.sessionEffort = effort
}

type agentEffortInput struct {
	Effort       string `json:"effort"`
	Model        string `json:"model"`
	SubagentType string `json:"subagent_type"`
}

// isRelativeEffort reports the experiment-gated relative Agent efforts the
// CLI either drops or applies as a one-tier step AO cannot reproduce.
func isRelativeEffort(effort string) bool {
	return effort == "lower" || effort == "higher"
}

// agentLaunchEffort is what the parser remembers per Agent launch: the
// stamps it wrote, the effort the launch named, the effort the launched
// agent runs at, and its model when known (a launch nested inside it
// inherits both).
type agentLaunchEffort struct {
	inherited string
	requested string
	named     string
	own       string
	model     string
}

// agentLaunchStamps are the launch-time facts a launch row records.
type agentLaunchStamps struct {
	inheritedEffort string
	requestedModel  string
}

// agentLaunchStamps returns the launch-time stamps for an Agent/Task launch
// (zero for any other tool) and records the launch for the launches nested
// inside it. A launch delivered again keeps its first answer.
func (p *Parser) agentLaunchStamps(toolName, toolUseID, parentToolUseID string, input json.RawMessage) agentLaunchStamps {
	if p == nil || toolUseID == "" || !isAgentLaunchToolName(toolName) {
		return agentLaunchStamps{}
	}
	if seen, ok := p.agentLaunchEfforts.get(toolUseID); ok {
		return agentLaunchStamps{inheritedEffort: seen.inherited, requestedModel: seen.requested}
	}
	inherited, launcherModel := p.launcherEffortAndModel(parentToolUseID)
	var decoded agentEffortInput
	if len(input) > 0 {
		_ = json.Unmarshal(input, &decoded)
	}
	launch := agentLaunchEffort{inherited: inherited, own: inherited}
	if strings.TrimSpace(decoded.SubagentType) == forkSubagentType {
		// A fork runs on its launcher's model at its launcher's effort.
		launch.model = launcherModel
	} else {
		if named := strings.TrimSpace(decoded.Effort); named != "" {
			launch.named = named
			launch.own = named
			if isRelativeEffort(named) {
				launch.own = ""
			}
		}
		if alias := strings.TrimSpace(decoded.Model); alias != "" {
			launch.requested = p.resolveAgentModel(alias, launcherModel)
			launch.model = launch.requested
		}
	}
	p.agentLaunchEfforts.set(toolUseID, launch)
	return agentLaunchStamps{inheritedEffort: launch.inherited, requestedModel: launch.requested}
}

// launcherEffortAndModel is the effort and model of the agent a launch or
// resume at parentToolUseID runs inside: the enclosing launch's when the
// parser saw it ("" for what it does not know), else the session's.
func (p *Parser) launcherEffortAndModel(parentToolUseID string) (string, string) {
	if parentToolUseID != "" {
		if parent, ok := p.agentLaunchEfforts.get(parentToolUseID); ok {
			return parent.own, parent.model
		}
	}
	return p.sessionEffort, p.model
}

// resolveAgentModel resolves an Agent launch's `model` alias the way the
// CLI does (2.1.300): an alias naming the launcher's own model family keeps
// the launcher's model, and any other alias takes the CLI's alias target.
// "" when the alias cannot be resolved here.
func (p *Parser) resolveAgentModel(alias, launcherModel string) string {
	if alias == "inherit" {
		return launcherModel
	}
	lower := strings.ToLower(alias)
	if launcherModel != "" && strings.Contains(strings.ToLower(launcherModel), lower) {
		return launcherModel
	}
	if target := p.modelAliases[provider.NormalizeModelSlug(string(provider.Claude), lower)]; target != "" {
		return target
	}
	if strings.HasPrefix(lower, "claude-") {
		return alias
	}
	return ""
}

// stampProjectedAgentLaunchEffort adds the stamps to a launch the session
// mirror projected, whose input rides in its meta.
func (p *Parser) stampProjectedAgentLaunchEffort(event *provider.ProviderEvent) {
	if !isAgentLaunchToolName(event.ItemType) {
		return
	}
	var fields map[string]json.RawMessage
	if len(event.Meta) > 0 && json.Unmarshal(event.Meta, &fields) != nil {
		return
	}
	stamps := p.agentLaunchStamps(event.ItemType, event.ItemID, event.ParentToolUseID, fields["input"])
	if stamps == (agentLaunchStamps{}) {
		return
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	stamps.apply(fields)
	if encoded, err := json.Marshal(fields); err == nil {
		event.Meta = encoded
	}
}

// apply writes the non-empty stamps into a meta field map.
func (s agentLaunchStamps) apply(fields map[string]json.RawMessage) {
	if s.inheritedEffort != "" {
		fields[inheritedEffortMetaKey], _ = json.Marshal(s.inheritedEffort)
	}
	if s.requestedModel != "" {
		fields[requestedModelMetaKey], _ = json.Marshal(s.requestedModel)
	}
}

// resumeInheritedEffort is the effort a §E6 resume of the agent launched at
// root runs at when its launch named none: its sender's effort now. It also
// moves the resumed agent's own effort, which launches nested in the resumed
// round inherit (their rows stay parented to root).
func (p *Parser) resumeInheritedEffort(senderParentToolUseID, root string) string {
	inherited, _ := p.launcherEffortAndModel(senderParentToolUseID)
	if root == "" {
		return inherited
	}
	if launch, ok := p.agentLaunchEfforts.get(root); ok && launch.named == "" {
		launch.own = inherited
		p.agentLaunchEfforts.set(root, launch)
	}
	return inherited
}
