// Page id → page component. Typed as a total record over SettingsSection,
// so adding a page to `sections.ts` without a component here is a compile
// error, and `fields.test.ts` mounts every page through this map, so the
// search index is checked against exactly what the router renders.

import type { Component } from 'svelte';
import type { SettingsSection } from './sections';
import ThemeSettings from './ThemeSettings.svelte';
import TypographySettings from './TypographySettings.svelte';
import ChatSettings from './ChatSettings.svelte';
import SpinnerSection from './SpinnerSection.svelte';
import ThreadSettings from './ThreadSettings.svelte';
import PerformanceSettings from './PerformanceSettings.svelte';
import KeybindingsSettings from './KeybindingsSettings.svelte';
import NotificationsSection from './NotificationsSection.svelte';
import UpdatesSettings from './UpdatesSettings.svelte';
import ClaudeSettings from './ClaudeSettings.svelte';
import CodexSettings from './CodexSettings.svelte';
import CommitMessageSettings from './CommitMessageSettings.svelte';
import BrowserSettings from './BrowserSettings.svelte';
import DiscussionsSettings from './DiscussionsSettings.svelte';
import ProjectsSettings from './ProjectsSettings.svelte';
import GitSettings from './GitSettings.svelte';
import EditorSection from './EditorSection.svelte';
import RemoteAccessSettings from './RemoteAccessSettings.svelte';
import SystemsSection from './SystemsSection.svelte';
import ObservabilitySettings from './ObservabilitySettings.svelte';
import StorageSettings from './StorageSettings.svelte';

export const SETTINGS_PAGES: Record<SettingsSection, Component> = {
  theme: ThemeSettings,
  typography: TypographySettings,
  chat: ChatSettings,
  spinner: SpinnerSection,
  threads: ThreadSettings,
  performance: PerformanceSettings,
  keybindings: KeybindingsSettings,
  notifications: NotificationsSection,
  updates: UpdatesSettings,
  claude: ClaudeSettings,
  codex: CodexSettings,
  'commit-messages': CommitMessageSettings,
  browser: BrowserSettings,
  discussions: DiscussionsSettings,
  projects: ProjectsSettings,
  git: GitSettings,
  editor: EditorSection,
  remote: RemoteAccessSettings,
  systems: SystemsSection,
  observability: ObservabilitySettings,
  storage: StorageSettings,
};
