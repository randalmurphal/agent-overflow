<script lang="ts">
  import X from 'lucide-svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import GeneralSettings from './GeneralSettings.svelte';
  import NetworkSection from './NetworkSection.svelte';
  import RemoteEndpointsSection from './RemoteEndpointsSection.svelte';
  import WSLSection from './WSLSection.svelte';
  import ProviderSettings from './ProviderSettings.svelte';
  import ArchivedThreads from './ArchivedThreads.svelte';
  import DiscussionsSettings from './DiscussionsSettings.svelte';
  import EditorSection from './EditorSection.svelte';
  import KeybindingsSettings from './KeybindingsSettings.svelte';
  import McpServersSettings from './McpServersSettings.svelte';
  import ObservabilitySettings from './ObservabilitySettings.svelte';
  import UpdatesSettings from './UpdatesSettings.svelte';
  import UpdateBadge from '../shared/UpdateBadge.svelte';
  import { SETTINGS_SECTIONS, type SettingsSection } from './sections';
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

  type ContextSettingsTarget = {
    threadId?: string;
    provider: string;
    model: string;
    contextWindow?: number;
    autoCompactStandardPercent?: number;
    autoCompactExtendedPercent?: number;
  } | null;

  let {
    onClose,
    initialSection = 'general',
    contextTarget = null,
  }: {
    onClose: () => void;
    initialSection?: SettingsSection;
    contextTarget?: ContextSettingsTarget;
  } = $props();

  let activeSection: SettingsSection = $state('general');

  $effect(() => {
    activeSection = initialSection;
  });

  function handleTabKeydown(e: KeyboardEvent) {
    const ids = SETTINGS_SECTIONS.map((s) => s.id);
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
      <MicroLabel as="p" class="tracking-[0.18em]">Preferences</MicroLabel>
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
    <div
      class="w-56 shrink-0 border-r border-border-subtle px-3 pt-5 pb-4 flex flex-col gap-0.5"
      role="tablist"
      aria-label="Settings Sections"
    >
      {#each SETTINGS_SECTIONS as section}
        <button
          id="settings-tab-{section.id}"
          onclick={() => activeSection = section.id}
          onkeydown={handleTabKeydown}
          role="tab"
          aria-selected={activeSection === section.id}
          aria-controls="settings-panel-{section.id}"
          tabindex={activeSection === section.id ? 0 : -1}
          class="w-full rounded-[var(--radius-field)] text-left px-3 py-1.5 text-[0.8125rem] cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40
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

    <div
      class="flex-1 overflow-y-auto px-8 py-6"
      role="tabpanel"
      id="settings-panel-{activeSection}"
      aria-labelledby="settings-tab-{activeSection}"
    >
      <div class="mx-auto max-w-3xl">
        {#if activeSection === 'general'}
          <GeneralSettings />
        {:else if activeSection === 'updates'}
          <UpdatesSettings />
        {:else if activeSection === 'providers'}
          <ProviderSettings contextTarget={contextTarget} />
        {:else if activeSection === 'editor'}
          <EditorSection />
        {:else if activeSection === 'network'}
          <NetworkSection />
          <!--
            WSLSection self-hides on non-WSL hosts and in --connect
            mode, so the "extra" composition under Network is a no-op
            on macOS and native Linux. Mounting it on the Network tab
            (rather than its own tab) keeps the sidebar list stable
            across platforms — adding a tab that's empty on most hosts
            would clutter the UX without buying anything.
          -->
          <div class="mt-10">
            <WSLSection />
          </div>
          <div class="mt-10">
            <RemoteEndpointsSection />
          </div>
        {:else if activeSection === 'discussions'}
          <DiscussionsSettings />
        {:else if activeSection === 'mcp'}
          <McpServersSettings />
        {:else if activeSection === 'keybindings'}
          <KeybindingsSettings />
        {:else if activeSection === 'observability'}
          <ObservabilitySettings />
        {:else if activeSection === 'archived'}
          <ArchivedThreads />
        {/if}
      </div>
    </div>
  </div>

  <footer class="border-t border-border-subtle px-5 py-2 text-[0.6875rem] text-fg-subtle shrink-0">
    Agent Overflow{appVersion ? ` v${appVersion}` : ''}
  </footer>
</div>
