// Single source of truth for the Settings panel's pages. SettingsView
// renders this list, `pages.ts` maps each id to its component, `fields.ts`
// anchors every searchable control to one of these ids, and
// `stores/settingsOverlay.svelte.ts` types its open target against
// SettingsSection, so a page is added in exactly one place and the nav, the
// router, the search index and the navigation type can't drift apart.
//
// Pages are clustered under a group label in the nav rail. `group` is part of
// the section shape rather than a parallel table so a new page cannot be
// added without placing it, and SETTINGS_SECTION_GROUPS / SETTINGS_SECTION_IDS
// are both derived from that one array — visual order and keyboard-nav order
// are the same list by construction, not by convention.
//
// One topic per page. `description` is the page's one-line subtitle, rendered
// above the page by SettingsView so a page component never repeats its own
// name; a page that needs a longer explanation puts it under a section
// header inside the page.

import { getProviderDefinition } from '../../providers/catalog';
import type { ProviderID } from '../../types/providers';

export const SETTINGS_GROUPS = ['Appearance', 'App', 'Agents', 'Workspace', 'Data'] as const;

export type SettingsGroup = (typeof SETTINGS_GROUPS)[number];

export const SETTINGS_SECTIONS = [
  {
    id: 'theme',
    label: 'Theme',
    group: 'Appearance',
    description:
      'Light or dark mode and the two theme axes. Themes are JSON files you or an agent can edit; the app reloads them as they are saved.',
  },
  {
    id: 'typography',
    label: 'Typography',
    group: 'Appearance',
    description: 'Typefaces and the base text size for the whole UI.',
  },
  {
    id: 'chat',
    label: 'Chat',
    group: 'Appearance',
    description: 'How messages, diffs, panes and activity render in a thread.',
  },
  {
    id: 'spinner',
    label: 'Working indicator',
    group: 'Appearance',
    description: 'What a thread shows while a turn is running.',
  },
  {
    id: 'threads',
    label: 'Threads',
    group: 'App',
    description: 'Defaults for new threads, and which destructive actions ask first.',
  },
  {
    id: 'performance',
    label: 'Performance',
    group: 'App',
    description: 'Streaming, rendering work, and keeping the machine awake while agents run.',
  },
  {
    id: 'keybindings',
    label: 'Keybindings',
    group: 'App',
    description:
      'Click a chord to re-bind; press the desired key combination or Escape to cancel. Shortcuts use mod as ⌘ on macOS and Ctrl elsewhere.',
  },
  {
    id: 'updates',
    label: 'Updates',
    group: 'App',
    description:
      'Check for and install new versions of Agent Overflow. Nothing is downloaded or installed without your confirmation.',
  },
  {
    id: 'claude',
    label: 'Claude Code',
    group: 'Agents',
    description:
      'The Claude Code CLI: binary, accounts, models and environment, plus the context, prompt, tool and session settings applied to every Claude thread.',
  },
  {
    id: 'codex',
    label: 'Codex',
    group: 'Agents',
    description:
      'The Codex CLI: binary, accounts, models and environment, plus the context, prompt and tool settings applied to every Codex thread.',
  },
  {
    id: 'commit-messages',
    label: 'Commit messages',
    group: 'Agents',
    description:
      'Which CLI writes commit messages, PR bodies and generated thread titles. Independent of the chat provider, so Claude users can still spend Codex cycles on short text.',
  },
  {
    id: 'browser',
    label: 'Browser',
    group: 'Agents',
    description: 'The built-in browser agents drive from a companion pane.',
  },
  {
    id: 'discussions',
    label: 'Discussions',
    group: 'Agents',
    description:
      'Multi-participant deliberations. Global definitions are available everywhere; project-scoped definitions take precedence for threads inside their project path.',
  },
  {
    id: 'projects',
    label: 'Projects',
    group: 'Workspace',
    description:
      'Per-project configuration. Settings here apply to every thread and workflow run in the selected project.',
  },
  {
    id: 'git',
    label: 'Git',
    group: 'Workspace',
    description: 'Background fetching, generated worktree branches, and self-hosted GitLab hosts.',
  },
  {
    id: 'editor',
    label: 'Editor',
    group: 'Workspace',
    description: 'Which editor opens when you click a file path in the chat.',
  },
  {
    id: 'remote',
    label: 'Remote access',
    group: 'Workspace',
    description:
      "Open this backend to other devices on your network, or attach this window to another machine's backend.",
  },
  {
    id: 'observability',
    label: 'Observability',
    group: 'Data',
    description: 'Tracing and event recording for diagnosing a bad turn after the fact.',
  },
  {
    id: 'storage',
    label: 'Storage',
    group: 'Data',
    description: 'Automatic cleanup and archived threads.',
  },
] as const satisfies ReadonlyArray<{
  id: string;
  label: string;
  group: SettingsGroup;
  description: string;
}>;

export type SettingsSection = (typeof SETTINGS_SECTIONS)[number]['id'];

export type SettingsSectionDef = (typeof SETTINGS_SECTIONS)[number];

/** The page users land on when nothing deep-links to a specific one. */
export const DEFAULT_SETTINGS_SECTION: SettingsSection = 'theme';

/**
 * Nav-rail clusters, in render order. A group whose pages have all been
 * removed is dropped rather than rendered as a bare label with nothing under
 * it.
 */
export const SETTINGS_SECTION_GROUPS = SETTINGS_GROUPS.map((label) => ({
  label,
  sections: SETTINGS_SECTIONS.filter((s) => s.group === label),
})).filter((group) => group.sections.length > 0);

/**
 * Every page id in visual order — derived from the groups, so arrow/Home/End
 * roving focus can never disagree with what the rail renders.
 */
export const SETTINGS_SECTION_IDS: readonly SettingsSection[] =
  SETTINGS_SECTION_GROUPS.flatMap((g) => g.sections.map((s) => s.id));

export function settingsSectionDef(id: SettingsSection): SettingsSectionDef {
  // The ids are a closed union derived from the array, so the lookup cannot
  // miss; the fallback only satisfies the type.
  return SETTINGS_SECTIONS.find((s) => s.id === id) ?? SETTINGS_SECTIONS[0];
}

/**
 * The page that configures a provider. A dependent provider (Claude TUI) has
 * no page of its own: its enable toggle rides on its parent's page, so a
 * deep link for it lands there. Throws on a provider with no page rather
 * than guessing, so a new provider cannot silently route to the wrong one.
 */
export function providerSettingsSection(provider: ProviderID): 'claude' | 'codex' {
  const root = getProviderDefinition(provider).dependsOnProvider ?? provider;
  if (root === 'claude' || root === 'codex') return root;
  throw new Error(`no settings page for provider ${provider}`);
}
