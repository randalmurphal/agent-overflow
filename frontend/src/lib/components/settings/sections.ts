// Single source of truth for the Settings panel's section tabs. SettingsView
// renders this list and `stores/settingsOverlay.svelte.ts` types its open
// target against SettingsSection, so a section is added in exactly one place
// and the tab list and the navigation type can't drift apart.
//
// Tabs are clustered under a group micro-label in the nav rail. `group` is part
// of the section shape rather than a parallel table so a new section cannot be
// added without placing it, and SETTINGS_SECTION_GROUPS / SETTINGS_SECTION_IDS
// are both derived from that one array — visual order and keyboard-nav order
// are the same list by construction, not by convention.

export const SETTINGS_GROUPS = ['App', 'Agents', 'Workspace', 'Data'] as const;

export type SettingsGroup = (typeof SETTINGS_GROUPS)[number];

export const SETTINGS_SECTIONS = [
  { id: 'general', label: 'General', group: 'App' },
  { id: 'keybindings', label: 'Keybindings', group: 'App' },
  { id: 'updates', label: 'Updates', group: 'App' },
  { id: 'providers', label: 'Providers', group: 'Agents' },
  { id: 'prompts', label: 'Prompts & Tools', group: 'Agents' },
  { id: 'browser', label: 'Browser', group: 'Agents' },
  { id: 'discussions', label: 'Discussions', group: 'Agents' },
  { id: 'projects', label: 'Projects', group: 'Workspace' },
  { id: 'git', label: 'Git', group: 'Workspace' },
  { id: 'editor', label: 'Editor', group: 'Workspace' },
  { id: 'network', label: 'Network', group: 'Workspace' },
  { id: 'systems', label: 'Systems', group: 'Workspace' },
  { id: 'observability', label: 'Observability', group: 'Data' },
  { id: 'storage', label: 'Storage', group: 'Data' },
] as const satisfies ReadonlyArray<{ id: string; label: string; group: SettingsGroup }>;

export type SettingsSection = (typeof SETTINGS_SECTIONS)[number]['id'];

/**
 * Nav-rail clusters, in render order. A group whose sections have all been
 * removed is dropped rather than rendered as a bare label with nothing under
 * it.
 */
export const SETTINGS_SECTION_GROUPS = SETTINGS_GROUPS.map((label) => ({
  label,
  sections: SETTINGS_SECTIONS.filter((s) => s.group === label),
})).filter((group) => group.sections.length > 0);

/**
 * Every tab id in visual order — derived from the groups, so arrow/Home/End
 * roving focus can never disagree with what the rail renders.
 */
export const SETTINGS_SECTION_IDS: readonly SettingsSection[] =
  SETTINGS_SECTION_GROUPS.flatMap((g) => g.sections.map((s) => s.id));
