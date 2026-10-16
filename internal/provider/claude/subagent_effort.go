package claude

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider"
)

// Subagent reasoning effort (claude-wire.md §"Agent effort").
//
// The Agent tool takes an optional `effort` input (Claude Code 2.1.292+).
// The CLI reports no effective effort for a subagent on the stream, so
// what AO records is what was true at launch: the input the launch carries,
// and `inherited_effort`, the effort the launching agent ran at. A launch
// that names no effort, and every fork, runs at its launcher's effort: the
// session's for a top-level launch, the enclosing subagent's for a nested
// one. The stamp is written once on the launch row because the session's
// effort can change afterwards (`/effort`).

// inheritedEffortMetaKey is the launch-row meta key of the stamp.
const inheritedEffortMetaKey = "inherited_effort"

// forkSubagentType is the subagent_type whose input effort the CLI ignores.
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

// observeEffortReply follows a `/effort` success reply. A tier AO cannot
// represent ("auto", "ultracode") leaves the session's effort unknown.
func (p *Parser) observeEffortReply(text string) {
	if !strings.HasPrefix(text, effortSetReplyPrefix) {
		return
	}
	tier, _ := EffortSetReplyTier(text)
	p.sessionEffort = tier
}

type agentEffortInput struct {
	Effort       string `json:"effort"`
	SubagentType string `json:"subagent_type"`
}

// agentLaunchEffort is what the parser remembers per Agent launch: the
// stamp it wrote, and the effort the launched agent runs at.
type agentLaunchEffort struct {
	inherited string
	own       string
}

// agentLaunchInheritedEffort returns the `inherited_effort` stamp for an
// Agent/Task launch ("" when unknown, or for any other tool) and records
// the effort the launched agent itself runs at, which a launch nested
// inside it inherits. A launch delivered again keeps its first answer.
func (p *Parser) agentLaunchInheritedEffort(toolName, toolUseID, parentToolUseID string, input json.RawMessage) string {
	if p == nil || toolUseID == "" || !isAgentLaunchToolName(toolName) {
		return ""
	}
	if seen, ok := p.agentLaunchEfforts.get(toolUseID); ok {
		return seen.inherited
	}
	inherited := p.sessionEffort
	if parentToolUseID != "" {
		if parent, ok := p.agentLaunchEfforts.get(parentToolUseID); ok {
			inherited = parent.own
		}
	}
	var decoded agentEffortInput
	if len(input) > 0 {
		_ = json.Unmarshal(input, &decoded)
	}
	own := inherited
	if requested := strings.TrimSpace(decoded.Effort); requested != "" && strings.TrimSpace(decoded.SubagentType) != forkSubagentType {
		own = requested
	}
	p.agentLaunchEfforts.set(toolUseID, agentLaunchEffort{inherited: inherited, own: own})
	return inherited
}

// stampProjectedAgentLaunchEffort adds the stamp to a launch the session
// mirror projected, whose input rides in its meta.
func (p *Parser) stampProjectedAgentLaunchEffort(event *provider.ProviderEvent) {
	if !isAgentLaunchToolName(event.ItemType) {
		return
	}
	var fields map[string]json.RawMessage
	if len(event.Meta) > 0 && json.Unmarshal(event.Meta, &fields) != nil {
		return
	}
	inherited := p.agentLaunchInheritedEffort(event.ItemType, event.ItemID, event.ParentToolUseID, fields["input"])
	if inherited == "" {
		return
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	fields[inheritedEffortMetaKey], _ = json.Marshal(inherited)
	if encoded, err := json.Marshal(fields); err == nil {
		event.Meta = encoded
	}
}
