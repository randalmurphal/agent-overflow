// Single source of truth for the Settings panel's section tabs. SettingsView
// renders this list (array order = tab order) and App.svelte types its
// openSettings target against SettingsSection, so a section is added in exactly
// one place and the tab list and the navigation type can't drift apart.

export const SETTINGS_SECTIONS = [
  { id: 'general', label: 'General' },
  { id: 'updates', label: 'Updates' },
  { id: 'providers', label: 'Providers' },
  { id: 'editor', label: 'Editor' },
  { id: 'projects', label: 'Projects' },
  { id: 'mcp', label: 'MCP Servers' },
  { id: 'network', label: 'Network' },
  { id: 'discussions', label: 'Discussions' },
  { id: 'keybindings', label: 'Keybindings' },
  { id: 'observability', label: 'Observability' },
  { id: 'archived', label: 'Archived' },
] as const;

export type SettingsSection = (typeof SETTINGS_SECTIONS)[number]['id'];
