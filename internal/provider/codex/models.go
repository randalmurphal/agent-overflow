package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

const (
	modelListTimeout   = 15 * time.Second
	modelListLimit     = 100
	modelListMaxPages  = 20
	modelListMaxModels = 1000

	// The two upstream spellings of "the fast tier", both sourced rather than
	// guessed. fastServiceTier is ServiceTier::Fast.request_value()
	// (codex-rs/protocol/src/config_types.rs) — the tier's canonical wire id.
	// legacyFastTier is SPEED_TIER_FAST (codex-rs/protocol/src/openai_models.rs),
	// which the wire uses in two places: as an entry of the deprecated
	// `additionalSpeedTiers` array, and as a `serviceTiers[].name`.
	// legacyFastTierName is the display name synthesized for the deprecated
	// path, which carries no name of its own.
	fastServiceTier    = "priority"
	legacyFastTier     = "fast"
	legacyFastTierName = "Fast"
)

type ModelListConfig struct {
	Binary       string
	WorkDir      string
	Env          map[string]string
	CustomModels []string
}

type codexModelListResponse struct {
	Data       []codexModel `json:"data"`
	NextCursor *string      `json:"nextCursor"`
}

type codexModel struct {
	Model                     string                      `json:"model"`
	DisplayName               string                      `json:"displayName"`
	Hidden                    bool                        `json:"hidden"`
	SupportedReasoningEfforts []codexReasoningEffortModel `json:"supportedReasoningEfforts"`
	DefaultReasoningEffort    string                      `json:"defaultReasoningEffort"`
	ServiceTiers              []codexModelServiceTier     `json:"serviceTiers"`
	// Deprecated by Codex in favor of ServiceTiers. Keep decoding it so AO
	// remains compatible with older app-server versions.
	AdditionalSpeedTiers []string `json:"additionalSpeedTiers"`
}

// codexModelServiceTier is one entry of a model's `serviceTiers` array, e.g.
// {"id":"priority","name":"Fast","description":"1.5x speed, increased usage"}.
// All three fields are carried: the id is what `serviceTier` must be on the
// wire, and the name/description are what the composer shows, so an upstream
// rename lands as a label change instead of breaking fast mode.
type codexModelServiceTier struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type codexReasoningEffortModel struct {
	ReasoningEffort string `json:"reasoningEffort"`
	Description     string `json:"description"`
}

// ListModels asks Codex app-server for its live model catalog. This is the
// provider-owned source of truth for model availability, supported reasoning
// efforts, default effort, and fast-mode support.
func ListModels(ctx context.Context, cfg ModelListConfig) ([]provider.ModelInfo, error) {
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > modelListTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, modelListTimeout)
		defer cancel()
	}

	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		binary = "codex"
	}

	client, err := newModelListClient(ctx, binary, cfg.WorkDir, cfg.Env)
	if err != nil {
		return nil, err
	}
	defer client.close()

	// Response-only client: one model/list round-trip, no notification
	// is ever awaited.
	if _, err := client.request(ctx, "initialize",
		codexInitializeParams("agent_overflow", oneShotOptOutNotificationMethods()),
	); err != nil {
		return nil, fmt.Errorf("codex: initialize for model/list: %w", err)
	}

	if err := client.notify("initialized", nil); err != nil {
		return nil, fmt.Errorf("codex: send initialized for model/list: %w", err)
	}

	models, err := client.listModels(ctx)
	if err != nil {
		return nil, err
	}
	return appendCustomModels(models, cfg.CustomModels), nil
}

type modelListClient struct {
	proc   *provider.Process
	nextID int64
}

func newModelListClient(ctx context.Context, binary, workDir string, env map[string]string) (*modelListClient, error) {
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary:   binary,
		Args:     codexAppServerArgs(),
		Dir:      workDir,
		Env:      env,
		UnsetEnv: []string{"CODEX_HOME"},
		Provider: string(provider.Codex),
	})
	if err != nil {
		return nil, fmt.Errorf("codex: spawn for model/list: %w", err)
	}
	return &modelListClient{proc: proc}, nil
}

func (c *modelListClient) close() {
	if c.proc != nil {
		_ = c.proc.Close()
	}
}

func (c *modelListClient) listModels(ctx context.Context) ([]provider.ModelInfo, error) {
	var models []provider.ModelInfo
	var cursor *string
	seenCursors := make(map[string]bool)

	for page := 0; page < modelListMaxPages; page++ {
		params := map[string]any{
			"includeHidden": false,
			"limit":         modelListLimit,
		}
		if cursor != nil {
			params["cursor"] = *cursor
		}

		raw, err := c.request(ctx, "model/list", params)
		if err != nil {
			return nil, fmt.Errorf("codex: model/list: %w", err)
		}

		var response codexModelListResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, fmt.Errorf("codex: decode model/list response: %w", err)
		}
		for _, model := range response.Data {
			if model.Hidden {
				continue
			}
			mapped := mapCodexModel(model)
			if mapped.Slug != "" {
				models = append(models, mapped)
				if len(models) > modelListMaxModels {
					return nil, fmt.Errorf("codex: model/list exceeded %d models", modelListMaxModels)
				}
			}
		}
		if response.NextCursor == nil || strings.TrimSpace(*response.NextCursor) == "" {
			return models, nil
		}
		nextCursor := strings.TrimSpace(*response.NextCursor)
		if seenCursors[nextCursor] {
			return nil, fmt.Errorf("codex: model/list repeated cursor %q", nextCursor)
		}
		seenCursors[nextCursor] = true
		cursor = response.NextCursor
	}
	return nil, fmt.Errorf("codex: model/list exceeded %d pages", modelListMaxPages)
}

func (c *modelListClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID

	message := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		message["params"] = params
	}

	data, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("codex: marshal %s: %w", method, err)
	}
	if err := c.proc.WriteLine(data); err != nil {
		return nil, err
	}

	for {
		line, err := c.readLine(ctx)
		if err != nil {
			return nil, err
		}

		var response struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &response); err != nil {
			return nil, fmt.Errorf("codex: decode %s response: %w", method, err)
		}
		if response.ID != id {
			continue
		}
		if response.Error != nil {
			return nil, fmt.Errorf("%s (code %d)", response.Error.Message, response.Error.Code)
		}
		return response.Result, nil
	}
}

func (c *modelListClient) notify(method string, params any) error {
	message := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		message["params"] = params
	}
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("codex: marshal %s notification: %w", method, err)
	}
	return c.proc.WriteLine(data)
}

func (c *modelListClient) readLine(ctx context.Context) ([]byte, error) {
	type readResult struct {
		line []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := c.proc.ReadLine()
		ch <- readResult{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-ch:
		if result.err != nil {
			if result.err == io.EOF {
				return nil, fmt.Errorf("codex: model/list app-server exited")
			}
			return nil, result.err
		}
		return result.line, nil
	}
}

func mapCodexModel(model codexModel) provider.ModelInfo {
	slug := strings.TrimSpace(model.Model)
	if slug == "" {
		return provider.ModelInfo{}
	}

	capabilities := []string(nil)
	fastTier := codexFastModeTier(model)
	if fastTier != nil {
		capabilities = append(capabilities, provider.ModelCapabilityFastMode)
	}

	return provider.ModelInfo{
		Slug:             slug,
		Name:             displayNameForCodexModel(model),
		Provider:         string(provider.Codex),
		Capabilities:     capabilities,
		FastModeTier:     fastTier,
		ContextWindows:   provider.ContextWindowOptionsForModel(string(provider.Codex), slug),
		ReasoningEfforts: mapCodexReasoningEfforts(model),
	}
}

// codexFastModeTier picks the service tier a fast-mode turn on this model runs
// on, or nil when the model has no fast tier — which is also the model's
// fast-mode support answer, since the two questions have one source.
//
// `serviceTiers` is the model's WHOLE tier menu, not a fast-tier list: upstream
// ships `flex` on the same enum (ServiceTier::Flex,
// codex-rs/protocol/src/config_types.rs) and its own fixtures carry
// {id:"batch",name:"slow"} alongside {id:"priority",name:"fast"}
// (codex-rs/tui/src/bottom_pane/slash_commands.rs). Treating "any declared
// tier" as the fast one would route a fast-mode turn onto a SLOWER tier, so the
// fast tier is identified, never assumed. Two anchors, either sufficient:
//
//   - id == "priority" — ServiceTier::Fast.request_value(), what upstream's own
//     ModelPreset::supports_fast_mode matches on.
//   - name == "fast", case-insensitive — SPEED_TIER_FAST, what upstream's TUI
//     matches on in current_model_fast_service_tier.
//
// Carrying both is what makes an upstream rename inert in either direction: a
// display-name change still matches the id, an id change still matches the
// name, and whichever entry matched supplies the id AO sends and the label the
// composer shows. Neither anchor can promote a non-fast tier.
//
// The deprecated `additionalSpeedTiers` list is consulted ONLY when
// `serviceTiers` is absent entirely: a model that declares tiers has already
// answered, and the older key must not override it.
func codexFastModeTier(model codexModel) *provider.FastModeTier {
	if tier := findCodexFastServiceTier(model.ServiceTiers); tier != nil {
		return &provider.FastModeTier{
			ID:          strings.TrimSpace(tier.ID),
			Name:        strings.TrimSpace(tier.Name),
			Description: strings.TrimSpace(tier.Description),
		}
	}
	if len(model.ServiceTiers) > 0 {
		return nil
	}
	if slices.Contains(model.AdditionalSpeedTiers, legacyFastTier) {
		return &provider.FastModeTier{ID: fastServiceTier, Name: legacyFastTierName}
	}
	return nil
}

// findCodexFastServiceTier returns the fast entry of a serviceTiers array, or
// nil. The id anchor wins over the name anchor so a catalog that renamed some
// other tier to "Fast" cannot displace the canonical one. Entries with a blank
// id are skipped: a tier AO cannot name on the wire is not one it can offer.
func findCodexFastServiceTier(tiers []codexModelServiceTier) *codexModelServiceTier {
	var byName *codexModelServiceTier
	for i := range tiers {
		if strings.TrimSpace(tiers[i].ID) == "" {
			continue
		}
		if tiers[i].ID == fastServiceTier {
			return &tiers[i]
		}
		if byName == nil && strings.EqualFold(strings.TrimSpace(tiers[i].Name), legacyFastTier) {
			byName = &tiers[i]
		}
	}
	return byName
}

func displayNameForCodexModel(model codexModel) string {
	name := strings.TrimSpace(model.DisplayName)
	if name != "" {
		return normalizeCodexDisplayName(name)
	}
	return normalizeCodexDisplayName(strings.TrimSpace(model.Model))
}

func normalizeCodexDisplayName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) >= 3 && strings.EqualFold(name[:3], "gpt") &&
		(len(name) == 3 || name[3] == '-' || name[3] == ' ') {
		remainder := strings.TrimLeft(name[3:], "- ")
		parts := strings.Fields(strings.ReplaceAll(remainder, "-", " "))
		for i := range parts {
			parts[i] = capitalizeCodexDisplayWord(parts[i])
		}
		if len(parts) == 0 {
			return "GPT"
		}
		return "GPT " + strings.Join(parts, " ")
	}

	return name
}

func capitalizeCodexDisplayWord(word string) string {
	if word == "" || word[0] < 'a' || word[0] > 'z' {
		return word
	}
	return string(word[0]-('a'-'A')) + word[1:]
}

func mapCodexReasoningEfforts(model codexModel) []provider.ReasoningEffortOption {
	options := make([]provider.ReasoningEffortOption, 0, len(model.SupportedReasoningEfforts))
	for _, effort := range model.SupportedReasoningEfforts {
		slug := strings.TrimSpace(effort.ReasoningEffort)
		if slug == "" {
			continue
		}
		options = append(options, provider.NewReasoningEffortOptionWithLabel(
			slug,
			"",
			slug == model.DefaultReasoningEffort,
		))
	}
	return options
}

func appendCustomModels(models []provider.ModelInfo, customModels []string) []provider.ModelInfo {
	if len(customModels) == 0 {
		return models
	}

	seen := make(map[string]struct{}, len(models)+len(customModels))
	for _, model := range models {
		seen[model.Slug] = struct{}{}
	}

	var template provider.ModelInfo
	if len(models) > 0 {
		template = models[0]
	}

	for _, raw := range customModels {
		slug := strings.TrimSpace(raw)
		if slug == "" {
			continue
		}
		if _, ok := seen[slug]; ok {
			continue
		}
		seen[slug] = struct{}{}

		// A user-added slug has no catalog entry of its own, so the first live
		// model is the template for every capability-shaped field. Cloning the
		// whole entry and then re-stamping identity is what keeps a new field
		// (fast-mode tier, and whatever comes next) inherited without a second
		// list to keep in sync — and CloneModelInfo is what keeps the two
		// entries from sharing a slice or tier pointer. A zero template (no
		// live models) clones to a zero value, leaving the custom entry bare.
		custom := provider.CloneModelInfo(template)
		custom.Slug = slug
		custom.Name = slug
		custom.Provider = string(provider.Codex)
		custom.IsCustom = true
		models = append(models, custom)
	}
	return models
}
