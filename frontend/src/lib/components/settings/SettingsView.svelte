<script lang="ts">
  import X from '@lucide/svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import GeneralSettings from './GeneralSettings.svelte';
  import DevicesSection from './DevicesSection.svelte';
  import NetworkSection from './NetworkSection.svelte';
  import RemoteEndpointsSection from './RemoteEndpointsSection.svelte';
  import WSLSection from './WSLSection.svelte';
  import ProviderSettings from './ProviderSettings.svelte';
  import PromptOverridesSettings from './PromptOverridesSettings.svelte';
  import BrowserSettings from './BrowserSettings.svelte';
  import GitSettings from './GitSettings.svelte';
  import StorageSettings from './StorageSettings.svelte';
  import DiscussionsSettings from './DiscussionsSettings.svelte';
  import EditorSection from './EditorSection.svelte';
  import ProjectsSettings from './ProjectsSettings.svelte';
  import KeybindingsSettings from './KeybindingsSettings.svelte';
  import ObservabilitySettings from './ObservabilitySettings.svelte';
  import UpdatesSettings from './UpdatesSettings.svelte';
  import UpdateBadge from '../shared/UpdateBadge.svelte';
  import {
    SETTINGS_SECTION_GROUPS,
    SETTINGS_SECTION_IDS,
    type SettingsSection,
  } from './sections';
  import { Version } from '../../stores/bindings';
  import { hasPendingUpdate } from '../../stores/updates.svelte';

  let appVersion = $state('');
  $effect(() => {
    Version()
      .then((v: string) => {
        appVersion = v;
      })
      .catch(() => {
        appVersion = '';
      });
  });

  let {
    onClose,
    initialSection = 'general',
  }: {
    onClose: () => void;
    initialSection?: SettingsSection;
  } = $props();

  let activeSection: SettingsSection = $state('general');

  $effect(() => {
    activeSection = initialSection;
  });

  function handleTabKeydown(e: KeyboardEvent) {
    const ids = SETTINGS_SECTION_IDS;
    const idx = ids.indexOf(activeSection);
    if (e.key === 'ArrowDown' || e.key === 'ArrowRight') {
      e.preventDefault();
      activeSection = ids[(idx + 1) % ids.length];
      focusActiveTab();
    } else if (e.key === 'ArrowUp' || e.key === 'ArrowLeft') {
      e.preventDefault();
      activeSection = ids[(idx - 1 + ids.length) % ids.length];
      focusActiveTab();
    } else if (e.key === 'Home') {
      e.preventDefault();
      activeSection = ids[0];
      focusActiveTab();
    } else if (e.key === 'End') {
      e.preventDefault();
      activeSection = ids[ids.length - 1];
      focusActiveTab();
    }
  }

  function focusActiveTab() {
    requestAnimationFrame(() => {
      const el = document.getElementById(`settings-tab-${activeSection}`);
      el?.focus();
    });
  }
</script>

<div class="flex h-full flex-col bg-transparent">
  <header class="flex items-center gap-2 border-b border-border-subtle px-5 py-3 shrink-0">
    <div>
      <MicroLabel as="p">Preferences</MicroLabel>
      <h2 class="mt-1 text-sm font-semibold text-fg">Settings</h2>
    </div>
    <button
      onclick={onClose}
      class="ml-auto text-fg-subtle hover:text-fg cursor-pointer p-1 rounded-[var(--radius-field)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
      aria-label="Close Settings"
    >
      <Icon icon={X} size={14} strokeWidth={2} class="opacity-90" />
    </button>
  </header>

  <div class="flex flex-1 min-h-0">
    <!--
      A `tablist` only admits `tab` children, so each cluster wraps in a
      `presentation` div (which drops out of the a11y tree, leaving the tabs as
      direct children) and the group micro-label is decorative: folding it into
      a tab's accessible name would rename every tab.
    -->
    <div
      class="w-56 shrink-0 border-r border-border-subtle px-3 pt-4 pb-4 flex flex-col gap-3 overflow-y-auto"
      role="tablist"
      aria-label="Settings Sections"
      aria-orientation="vertical"
    >
      {#each SETTINGS_SECTION_GROUPS as group (group.label)}
        <div role="presentation" class="flex flex-col gap-0.5">
          <MicroLabel as="p" class="px-3 pb-1" decorative>{group.label}</MicroLabel>
          {#each group.sections as section (section.id)}
            <button
              id="settings-tab-{section.id}"
              onclick={() => (activeSection = section.id)}
              onkeydown={handleTabKeydown}
              role="tab"
              aria-selected={activeSection === section.id}
              aria-controls="settings-panel-{section.id}"
              tabindex={activeSection === section.id ? 0 : -1}
              class="w-full rounded-[var(--radius-field)] text-left px-3 py-1 text-[0.8125rem] cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40
                {activeSection === section.id
                  ? 'bg-accent/10 text-fg font-medium'
                  : 'text-fg-muted hover:text-fg hover:bg-surface-2/30'}"
            >
              {section.label}
              {#if section.id === 'updates' && hasPendingUpdate()}
                <UpdateBadge />
              {/if}
            </button>
          {/each}
        </div>
      {/each}
    </div>

    <div
      class="flex-1 overflow-y-auto px-8 py-6"
      role="tabpanel"
      id="settings-panel-{activeSection}"
      aria-labelledby="settings-tab-{activeSection}"
    >
      <div class="mx-auto max-w-3xl">
        {#if activeSection === 'general'}
          <GeneralSettings />
        {:else if activeSection === 'keybindings'}
          <KeybindingsSettings />
        {:else if activeSection === 'updates'}
          <UpdatesSettings />
        {:else if activeSection === 'providers'}
          <ProviderSettings />
        {:else if activeSection === 'prompts'}
          <PromptOverridesSettings />
        {:else if activeSection === 'browser'}
          <BrowserSettings />
        {:else if activeSection === 'discussions'}
          <DiscussionsSettings />
        {:else if activeSection === 'projects'}
          <ProjectsSettings />
        {:else if activeSection === 'git'}
          <GitSettings />
        {:else if activeSection === 'editor'}
          <EditorSection />
        {:else if activeSection === 'network'}
          <!--
            WSLSection self-hides on non-WSL hosts and in --connect
            mode, so the "extra" composition under Network is a no-op
            on macOS and native Linux. Mounting it on the Network tab
            (rather than its own tab) keeps the sidebar list stable
            across platforms — adding a tab that's empty on most hosts
            would clutter the UX without buying anything.
          -->
          <div class="flex flex-col gap-6">
            <NetworkSection />
            <DevicesSection />
            <WSLSection />
            <RemoteEndpointsSection />
          </div>
        {:else if activeSection === 'observability'}
          <ObservabilitySettings />
        {:else if activeSection === 'storage'}
          <StorageSettings />
        {/if}
      </div>
    </div>
  </div>

  <footer class="border-t border-border-subtle px-5 py-2 text-[0.6875rem] text-fg-subtle shrink-0">
    Agent Overflow{appVersion ? ` v${appVersion}` : ''}
  </footer>
</div>
