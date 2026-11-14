package claude

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// Subagent reasoning effort (claude-wire.md §"Agent effort").
//
// The Agent tool takes an optional `effort` input (Claude Code 2.1.292+).
// The headless stream reports no effective effort for a subagent, so AO
// records what it can prove: the input the launch carries, and
// `inherited_effort`, the effort the agent runs at when the launch names
// none. An effort a launch names is a layer every launch nested in it
// inherits, and a fork runs at its launcher's effort. Otherwise (2.1.317+)
// the session's effort applies only to an agent on the model it was picked
// for, the main loop's model when the outermost launch ran. An agent on
// another model runs at that model's effort from the user's Claude settings
// or its default, which AO cannot see, so it is not stamped. The agent's
// model is known once its first envelope reports it, so the stamp rides
// that report. Before 2.1.317 every such launch runs at its launcher's
// effort and is stamped at launch. A §E6 resume follows the same rule with
// the session's effort at resume time. Each stamp is written once on its
// row because the session's effort can change afterwards (`/effort`).
//
// claude-tui reconstructs each subagent request's `output_config.effort`,
// the effort the CLI sent, so a claude-tui agent also records `run_effort`.
//
// The launch's requested model alias is resolved here as well
// (`requested_model`), because the alias's target decides which effort the
// model can run at before the subagent reports its model.

const (
	// inheritedEffortMetaKey is the launch- and carrier-row meta key of the
	// launcher's effort.
	inheritedEffortMetaKey = "inherited_effort"
	// inheritedEffortAlternativesMetaKey lists the higher tiers the launcher
	// may have been at instead of `inherited_effort`: claude-tui learns the
	// session's effort from the main model's request, which the CLI has
	// clamped to what that model offers.
	inheritedEffortAlternativesMetaKey = "inherited_effort_alternatives"
	// requestedModelMetaKey is the launch-row meta key of the model the
	// launch's `model` alias resolves to.
	requestedModelMetaKey = "requested_model"
	// runEffortMetaKey is the launch-row meta key of the effort a
	// claude-tui subagent's first request ran at, "" for none.
	runEffortMetaKey = "run_effort"
	// RequestEffortKey is the claude-tui reconstructor's top-level field on
	// an assistant envelope: the `output_config.effort` of the API request
	// that produced it, "" when the request named none.
	RequestEffortKey = "ao_request_effort"
)

// modelScopedEffortCLIVersion is the first Claude Code build that runs a
// subagent on a model other than the one the session's effort was picked
// for at that model's own effort (2.1.317 changelog).
const modelScopedEffortCLIVersion = "2.1.317"

// SetCLIVersion seeds the CLI version that selects the subagent effort
// rule; `system/init` overrides it. An unknown version takes the current
// rule, under which AO stamps less, never a wrong effort.
func (p *Parser) SetCLIVersion(version string) {
	if p == nil {
		return
	}
	p.cliVersion = strings.TrimSpace(version)
}

// modelScopedEffort reports whether the CLI applies the session's effort
// only to agents on the model it was picked for.
func (p *Parser) modelScopedEffort() bool {
	if _, ok := parseClaudeCLIVersion(p.cliVersion); !ok {
		return true
	}
	return claudeCLIVersionAtLeast(p.cliVersion, modelScopedEffortCLIVersion)
}

// observeMainLoopModel follows the model of the main loop's responses, the
// model the session's effort is picked for when an outermost launch runs.
func (p *Parser) observeMainLoopModel(parentToolUseID, model string) {
	model = strings.TrimSpace(model)
	if p == nil || parentToolUseID != "" || model == "" || isSyntheticCLIModel(model) {
		return
	}
	p.mainLoopModel = model
}

// pickedEffortModel is the model the session's effort applies to now.
func (p *Parser) pickedEffortModel() string {
	if p.mainLoopModel != "" {
		return p.mainLoopModel
	}
	return p.model
}

// sameEffortModel compares models the way the CLI does: one ID, with or
// without its context marker. A model either side does not know is not the
// same.
func sameEffortModel(a, b string) bool {
	a = strings.ToLower(provider.NormalizeModelSlug(string(provider.Claude), strings.TrimSpace(a)))
	b = strings.ToLower(provider.NormalizeModelSlug(string(provider.Claude), strings.TrimSpace(b)))
	return a != "" && a == b
}

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
	p.sessionEffortAlternatives = nil
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
	if tier, ok := effortReplyTier(text); ok {
		p.sessionEffort = tier
		p.sessionEffortAlternatives = nil
	}
}

// effortReplyTier is the session's effort after a `/effort` reply, "" when
// AO cannot name it; ok is false for any other text.
func effortReplyTier(text string) (string, bool) {
	if strings.HasPrefix(text, effortSetReplyPrefix) {
		tier, _ := EffortSetReplyTier(text)
		return tier, true
	}
	if match := effortCapReplyPattern.FindStringSubmatch(text); match != nil {
		if IsLiveEffortTier(match[1]) {
			return match[1], true
		}
		return "", true
	}
	for _, prefix := range effortClearedReplyPrefixes {
		if strings.HasPrefix(text, prefix) {
			return "", true
		}
	}
	return "", false
}

// observeRequestEffort follows the effort of the main-loop API request a
// claude-tui assistant envelope came from. The terminal's `/effort` never
// reaches the parser, but every main-loop request names the effort it runs
// at, so this is the TUI's record of the session's effort. The CLI clamps
// the session's effort to the request's model (an xhigh or max the model
// lacks is sent as high), so a high request on such a model leaves the
// session's effort ambiguous between high and the tiers the model lacks.
func (p *Parser) observeRequestEffort(raw map[string]json.RawMessage, parentToolUseID, model string) {
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
	p.sessionEffortAlternatives = nil
	if effort == "high" {
		p.sessionEffortAlternatives = clampedAwayTiers(model)
	}
}

// clampedAwayTiers are the tiers the CLI sends as high for model: xhigh
// and max when the model lacks them (2.1.300 effort clamp), both when
// the catalog does not know the model.
func clampedAwayTiers(model string) []string {
	info, known := provider.FindModel(string(provider.Claude), model)
	var tiers []string
	for _, tier := range []string{"xhigh", "max"} {
		if !known || !provider.ModelInfoSupportsReasoningEffort(info, tier) {
			tiers = append(tiers, tier)
		}
	}
	return tiers
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
// agent runs at ("" unknown), and its model when known (a launch nested
// inside it inherits from it).
type agentLaunchEffort struct {
	inherited             string
	inheritedAlternatives []string
	requested             string
	named                 string
	own                   string
	ownAlternatives       []string
	model                 string
	// layered: own comes from an effort a launch named, this one or one it
	// is nested in, which nested launches inherit whatever their model.
	layered bool
	// pickedModel is the model the session's effort was picked for when the
	// outermost launch above this one ran.
	pickedModel string
	// pending holds the session's effort at launch until the agent reports
	// its model (2.1.317+): it applies on pickedModel only.
	pending             bool
	pendingEffort       string
	pendingAlternatives []string
	modelReported       bool
	// ran is the effort the agent's mirrored request reported before the
	// launch itself was delivered (holdRunEffort).
	ran string
}

// agentLaunchStamps are the launch-time facts a launch row records.
type agentLaunchStamps struct {
	inheritedEffort             string
	inheritedEffortAlternatives []string
	requestedModel              string
	runEffort                   string
}

// agentLauncher is what a launch inherits from the agent it runs inside.
type agentLauncher struct {
	effort       string
	alternatives []string
	model        string
	layered      bool
	pickedModel  string
}

// agentLaunchStamps returns the launch-time stamps for an Agent/Task launch
// (zero for any other tool) and records the launch for the launches nested
// inside it. A launch delivered again keeps its first answer.
func (p *Parser) agentLaunchStamps(toolName, toolUseID, parentToolUseID string, input json.RawMessage) agentLaunchStamps {
	if p == nil || toolUseID == "" || !isAgentLaunchToolName(toolName) {
		return agentLaunchStamps{}
	}
	if seen, ok := p.agentLaunchEfforts.get(toolUseID); ok {
		return seen.stamps()
	}
	launcher := p.launcherOf(parentToolUseID)
	var decoded agentEffortInput
	if len(input) > 0 {
		_ = json.Unmarshal(input, &decoded)
	}
	launch := agentLaunchEffort{
		inherited: launcher.effort, inheritedAlternatives: launcher.alternatives,
		own: launcher.effort, ownAlternatives: launcher.alternatives,
		layered: launcher.layered, pickedModel: launcher.pickedModel,
	}
	fork := strings.TrimSpace(decoded.SubagentType) == forkSubagentType
	if !fork && !launcher.layered && p.modelScopedEffort() {
		// The session's effort applies only if the agent turns out to run
		// on the picked model (agentModelReported).
		launch.pending = true
		launch.pendingEffort, launch.pendingAlternatives = p.sessionEffort, p.sessionEffortAlternatives
		launch.inherited, launch.inheritedAlternatives = "", nil
		launch.own, launch.ownAlternatives = "", nil
	}
	if fork {
		// A fork runs on its launcher's model at its launcher's effort.
		launch.model = launcher.model
	} else {
		if named := strings.TrimSpace(decoded.Effort); named != "" {
			launch.named = named
			launch.own = named
			launch.ownAlternatives = nil
			launch.layered = true
			if isRelativeEffort(named) {
				launch.own = ""
			}
		}
		if alias := strings.TrimSpace(decoded.Model); alias != "" {
			launch.requested = p.resolveAgentModel(alias, launcher.model)
			launch.model = launch.requested
		}
	}
	if effort, ok := p.heldRunEfforts.get(toolUseID); ok {
		p.heldRunEfforts.delete(toolUseID)
		launch.ran = effort
		p.runEffortReported.set(toolUseID, struct{}{})
	}
	p.agentLaunchEfforts.set(toolUseID, launch)
	return launch.stamps()
}

// agentModelReported takes the model the agent launched at launchID
// reports on its first envelope and returns the stamps that report
// settles: `inherited_effort` when the agent runs on the model the
// session's effort was picked for. Zero for a launch the parser did not see
// or already settled.
func (p *Parser) agentModelReported(launchID, model string) agentLaunchStamps {
	launch, ok := p.agentLaunchEfforts.get(launchID)
	if !ok || launch.modelReported {
		return agentLaunchStamps{}
	}
	launch.modelReported = true
	launch.model = model
	var settled agentLaunchStamps
	if launch.pending {
		launch.pending = false
		if sameEffortModel(model, launch.pickedModel) {
			launch.inherited, launch.inheritedAlternatives = launch.pendingEffort, launch.pendingAlternatives
			if !launch.layered {
				launch.own, launch.ownAlternatives = launch.pendingEffort, launch.pendingAlternatives
			}
			settled.inheritedEffort = launch.inherited
			settled.inheritedEffortAlternatives = launch.inheritedAlternatives
		}
		launch.pendingEffort, launch.pendingAlternatives = "", nil
	}
	p.agentLaunchEfforts.set(launchID, launch)
	return settled
}

// subagentModelReport is the meta of the update that records the model the
// agent launched at launchID reports on its first envelope, with the effort
// stamps that report settles and, from a claude-tui envelope, the effort
// its request ran at.
func (p *Parser) subagentModelReport(raw map[string]json.RawMessage, launchID, model string) map[string]json.RawMessage {
	fields := map[string]json.RawMessage{}
	fields["subagent_model"], _ = json.Marshal(model)
	p.agentModelReported(launchID, model).apply(fields)
	if value, ok := raw[RequestEffortKey]; ok {
		var effort string
		_ = json.Unmarshal(value, &effort)
		// "" is a request that named no effort; a tier AO cannot name is
		// left unrecorded.
		if _, done := p.runEffortReported.get(launchID); !done {
			if effort = strings.TrimSpace(effort); effort == "" || IsLiveEffortTier(effort) {
				fields[runEffortMetaKey], _ = json.Marshal(effort)
				p.runEffortReported.set(launchID, struct{}{})
			}
		}
	}
	return fields
}

// mirroredRunEffort is one mirrored request row that names an effort tier.
type mirroredRunEffort struct {
	uuid   string
	effort string
}

// observeMirroredRunEfforts records the effort an agent's request ran at,
// as its session-mirror rows report it, on the row the agent's task is
// bound to now: its launch, or a §E6 resume carrier for a resumed round.
// Before task_started binds the agent, agent_metadata's toolUseId names the
// launch. The mirror can deliver a row again, after its agent was bound to
// a later round, so each source row counts once.
func (p *Parser) observeMirroredRunEfforts(threadID, agentID, metadataToolUseID string, rows []mirroredRunEffort, now time.Time) []provider.ProviderEvent {
	if agentID == "" {
		return nil
	}
	var events []provider.ProviderEvent
	for _, row := range rows {
		if row.uuid != "" {
			if _, seen := p.runEffortSourceRows.get(row.uuid); seen {
				continue
			}
			p.runEffortSourceRows.set(row.uuid, struct{}{})
		}
		rowID := firstNonEmpty(p.taskToolUseRef(agentID).ToolUseID, metadataToolUseID)
		events = append(events, p.recordRunEffort(threadID, agentID, rowID, row.effort, now)...)
	}
	return events
}

// takePendingRunEffort records the effort an agent's rows reported before
// its task_started bound it to rowID.
func (p *Parser) takePendingRunEffort(threadID, agentID, rowID string, now time.Time) []provider.ProviderEvent {
	effort, ok := p.heldRunEfforts.get(agentID)
	if !ok {
		return nil
	}
	p.heldRunEfforts.delete(agentID)
	return p.recordRunEffort(threadID, agentID, rowID, effort, now)
}

// recordRunEffort is the meta-only update that records run_effort on rowID,
// once per row, when rowID is an Agent launch or the resume carrier of the
// agent's task. An effort whose row is not known yet is held: under the
// agent until task_started binds it (takePendingRunEffort), under rowID
// until the mirror delivers that launch (agentLaunchStamps).
func (p *Parser) recordRunEffort(threadID, agentID, rowID, effort string, now time.Time) []provider.ProviderEvent {
	if rowID == "" {
		p.holdRunEffort(agentID, effort)
		return nil
	}
	if _, done := p.runEffortReported.get(rowID); done {
		return nil
	}
	_, launch := p.agentLaunchEfforts.get(rowID)
	root := p.taskTranscriptRoot(agentID)
	if carrier := root != "" && root != rowID; !launch && !carrier {
		p.holdRunEffort(rowID, effort)
		return nil
	}
	p.runEffortReported.set(rowID, struct{}{})
	meta, _ := json.Marshal(map[string]any{"meta_update_only": true, runEffortMetaKey: effort})
	return []provider.ProviderEvent{{
		Kind:      provider.EventToolStart,
		ThreadID:  threadID,
		ItemID:    rowID,
		Meta:      meta,
		Timestamp: now,
	}}
}

// holdRunEffort keeps the first effort reported for key, an agent id or a
// tool use id, until its row is known.
func (p *Parser) holdRunEffort(key, effort string) {
	if _, held := p.heldRunEfforts.get(key); !held {
		p.heldRunEfforts.set(key, effort)
	}
}

func (l agentLaunchEffort) stamps() agentLaunchStamps {
	return agentLaunchStamps{
		inheritedEffort:             l.inherited,
		inheritedEffortAlternatives: l.inheritedAlternatives,
		requestedModel:              l.requested,
		runEffort:                   l.ran,
	}
}

// launcherOf is what a launch or resume at parentToolUseID inherits from
// the agent it runs inside: the enclosing launch's when the parser saw it
// ("" for an effort it does not know), else the session's.
func (p *Parser) launcherOf(parentToolUseID string) agentLauncher {
	if parentToolUseID != "" {
		if parent, ok := p.agentLaunchEfforts.get(parentToolUseID); ok {
			return agentLauncher{
				effort: parent.own, alternatives: parent.ownAlternatives, model: parent.model,
				layered: parent.layered, pickedModel: parent.pickedModel,
			}
		}
	}
	picked := p.pickedEffortModel()
	return agentLauncher{
		effort: p.sessionEffort, alternatives: p.sessionEffortAlternatives,
		model: picked, pickedModel: picked,
	}
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
	if len(stamps.fields()) == 0 {
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

// fields are the non-empty stamps by meta key.
func (s agentLaunchStamps) fields() map[string]any {
	fields := map[string]any{}
	if s.inheritedEffort != "" {
		fields[inheritedEffortMetaKey] = s.inheritedEffort
		if len(s.inheritedEffortAlternatives) > 0 {
			fields[inheritedEffortAlternativesMetaKey] = s.inheritedEffortAlternatives
		}
	}
	if s.requestedModel != "" {
		fields[requestedModelMetaKey] = s.requestedModel
	}
	if s.runEffort != "" {
		fields[runEffortMetaKey] = s.runEffort
	}
	return fields
}

// apply writes the non-empty stamps into a meta field map.
func (s agentLaunchStamps) apply(fields map[string]json.RawMessage) {
	for key, value := range s.fields() {
		fields[key], _ = json.Marshal(value)
	}
}

// resumeInheritedEffort is the effort a §E6 resume of the agent launched at
// root runs at when its launch named none, with the higher tiers it may be
// instead: its sender's effort layer, else (2.1.317+) the session's effort
// now when the agent runs on the model its launch picked the effort for;
// before 2.1.317, its sender's effort. It also moves the resumed agent's
// own effort, which launches nested in the resumed round inherit (their
// rows stay parented to root).
func (p *Parser) resumeInheritedEffort(senderParentToolUseID, root string) (string, []string) {
	sender := p.launcherOf(senderParentToolUseID)
	inherited, alternatives := sender.effort, sender.alternatives
	launch, known := p.agentLaunchEfforts.get(root)
	if !sender.layered && p.modelScopedEffort() {
		inherited, alternatives = "", nil
		if known && launch.modelReported && sameEffortModel(launch.model, launch.pickedModel) {
			inherited, alternatives = p.sessionEffort, p.sessionEffortAlternatives
		}
	}
	if root != "" && known && launch.named == "" {
		launch.own = inherited
		launch.ownAlternatives = alternatives
		launch.layered = sender.layered
		p.agentLaunchEfforts.set(root, launch)
	}
	return inherited, alternatives
}

// AgentLaunchRecord is what the thread's history holds about an agent
// launch an earlier process ran (Config.AgentLaunchByTask).
type AgentLaunchRecord struct {
	// ToolUseID is the launch's tool_use id, the agent's transcript root.
	ToolUseID string
	// Effort is the effort the launch named ("" for none, and for a fork,
	// whose input effort the CLI ignores). A resume runs at it.
	Effort string
	// Model is the model the agent ran on, when the history knows it.
	Model string
}

// rememberHistoricalLaunch records a launch only the thread's history
// holds, so a resume of it, and the launches nested in the resumed round,
// run at the effort it named.
func (p *Parser) rememberHistoricalLaunch(record AgentLaunchRecord) {
	if record.ToolUseID == "" {
		return
	}
	if _, ok := p.agentLaunchEfforts.get(record.ToolUseID); ok {
		return
	}
	launch := agentLaunchEffort{named: record.Effort, model: record.Model, layered: record.Effort != ""}
	if !isRelativeEffort(record.Effort) {
		launch.own = record.Effort
	}
	p.agentLaunchEfforts.set(record.ToolUseID, launch)
}
