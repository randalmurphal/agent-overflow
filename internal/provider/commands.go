package provider

// SlashCommand is one command the provider CLI executes itself, without an
// API call — Claude's built-ins (`/usage`, `/context`), user and project
// commands, skills, plugin commands, and MCP prompts.
//
// Name is WITHOUT the leading slash, matching every wire surface that reports
// one (the `initialize` control_response's `commands[]`, `system/init`'s
// `slash_commands[]`, and `system/commands_changed`). Description and
// ArgumentHint are optional: `system/init` reports names only, so an entry
// sourced from it carries neither and absence must never be rendered as "this
// command has no description".
type SlashCommand struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	ArgumentHint string `json:"argumentHint,omitempty"`
}

// PluginInfo is one entry of `system/init`'s `plugins[]`. Reported as data —
// AO neither loads nor validates plugins; the list exists so a UI can say
// where a plugin-provided command came from.
type PluginInfo struct {
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	Source string `json:"source,omitempty"`
}
