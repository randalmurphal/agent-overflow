package codex

import "agent-overflow/internal/provider"

func codexCollaborationMode(mode provider.InteractionMode, model, effort string) map[string]any {
	normalized := provider.NormalizeInteractionMode(string(mode))
	modeKind := "default"
	if normalized == provider.ModePlan {
		modeKind = "plan"
	}
	settings := map[string]any{
		"developer_instructions": nil,
	}
	if model != "" {
		settings["model"] = model
	}
	if effort != "" {
		settings["reasoning_effort"] = effort
	}
	return map[string]any{
		"mode":     modeKind,
		"settings": settings,
	}
}
