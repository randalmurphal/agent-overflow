// Lazy, cached loaders for each Settings section so opening Settings parses
// only the active page instead of all section components up front, while
// keeping warm tab revisits synchronous via `peek()`.
//
// `pages.ts` stays as the static test map for `fields.test.ts`, which mounts
// every section synchronously to verify the settings search index.

import {
  createCachedComponentLoader,
  createEagerComponentLoader,
  type CachedComponentLoader,
} from '../../utils/lazyComponent';
import type { SettingsSection } from './sections';
import ThemeSettings from './ThemeSettings.svelte';
import UpdatesSettings from './UpdatesSettings.svelte';

export const SETTINGS_PAGE_LOADERS: Record<SettingsSection, CachedComponentLoader<Record<string, never>>> = {
  theme: createEagerComponentLoader(ThemeSettings),
  typography: createCachedComponentLoader(() => import('./TypographySettings.svelte')),
  chat: createCachedComponentLoader(() => import('./ChatSettings.svelte')),
  spinner: createCachedComponentLoader(() => import('./SpinnerSection.svelte')),
  threads: createCachedComponentLoader(() => import('./ThreadSettings.svelte')),
  sidebar: createCachedComponentLoader(() => import('./SidebarSettings.svelte')),
  performance: createCachedComponentLoader(() => import('./PerformanceSettings.svelte')),
  keybindings: createCachedComponentLoader(() => import('./KeybindingsSettings.svelte')),
  notifications: createCachedComponentLoader(() => import('./NotificationsSection.svelte')),
  voice: createCachedComponentLoader(() => import('./VoiceSettings.svelte')),
  updates: createEagerComponentLoader(UpdatesSettings),
  claude: createCachedComponentLoader(() => import('./ClaudeSettings.svelte')),
  codex: createCachedComponentLoader(() => import('./CodexSettings.svelte')),
  agy: createCachedComponentLoader(() => import('./AgySettings.svelte')),
  'commit-messages': createCachedComponentLoader(() => import('./CommitMessageSettings.svelte')),
  browser: createCachedComponentLoader(() => import('./BrowserSettings.svelte')),
  'thread-tools': createCachedComponentLoader(() => import('./ThreadToolsSettings.svelte')),
  assistants: createCachedComponentLoader(() => import('./AssistantsSettings.svelte')),
  'agent-profiles': createCachedComponentLoader(() => import('./AgentProfilesSettings.svelte')),
  'agent-guide': createCachedComponentLoader(() => import('./AgentGuideSettings.svelte')),
  memory: createCachedComponentLoader(() => import('./MemorySettings.svelte')),
  projects: createCachedComponentLoader(() => import('./ProjectsSettings.svelte')),
  git: createCachedComponentLoader(() => import('./GitSettings.svelte')),
  editor: createCachedComponentLoader(() => import('./EditorSection.svelte')),
  'tracker-connections': createCachedComponentLoader(() => import('./TrackerConnectionsSettings.svelte')),
  'tracker-boards': createCachedComponentLoader(() => import('./TrackerBoardsSettings.svelte')),
  'tracker-statuses': createCachedComponentLoader(() => import('./TrackerStatusesSettings.svelte')),
  'tracker-fields': createCachedComponentLoader(() => import('./TrackerFieldsSettings.svelte')),
  'tracker-start-work': createCachedComponentLoader(() => import('./TrackerStartWorkSettings.svelte')),
  'tracker-agents': createCachedComponentLoader(() => import('./TrackerAgentsSettings.svelte')),
  remote: createCachedComponentLoader(() => import('./RemoteAccessSettings.svelte')),
  systems: createCachedComponentLoader(() => import('./SystemsSection.svelte')),
  'agent-access': createCachedComponentLoader(() => import('./AgentAccessSettings.svelte')),
  observability: createCachedComponentLoader(() => import('./ObservabilitySettings.svelte')),
  storage: createCachedComponentLoader(() => import('./StorageSettings.svelte')),
};
