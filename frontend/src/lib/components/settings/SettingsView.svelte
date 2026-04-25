<script lang="ts">
  import X from 'lucide-svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import GeneralSettings from './GeneralSettings.svelte';
  import NetworkSection from './NetworkSection.svelte';
  import RemoteEndpointsSection from './RemoteEndpointsSection.svelte';
  import ProviderSettings from './ProviderSettings.svelte';
  import ArchivedThreads from './ArchivedThreads.svelte';
  import DiscussionsSettings from './DiscussionsSettings.svelte';
  import EditorSection from './EditorSection.svelte';
  import KeybindingsSettings from './KeybindingsSettings.svelte';
  import ObservabilitySettings from './ObservabilitySettings.svelte';

  let { onClose }: { onClose: () => void } = $props();

  type Section =
    | 'general'
    | 'providers'
    | 'editor'
    | 'network'
    | 'discussions'
    | 'keybindings'
    | 'observability'
    | 'archived';
  let activeSection: Section = $state('general');

  const sections: Array<{ id: Section; label: string }> = [
    { id: 'general', label: 'General' },
    { id: 'providers', label: 'Providers' },
    { id: 'editor', label: 'Editor' },
    { id: 'network', label: 'Network' },
    { id: 'discussions', label: 'Discussions' },
    { id: 'keybindings', label: 'Keybindings' },
    { id: 'observability', label: 'Observability' },
    { id: 'archived', label: 'Archived' },
  ];

  function handleTabKeydown(e: KeyboardEvent) {
    const ids = sections.map((s) => s.id);
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
      {#each sections as section}
        <button
          id="settings-tab-{section.id}"
          onclick={() => activeSection = section.id}
          onkeydown={handleTabKeydown}
          role="tab"
          aria-selected={activeSection === section.id}
          aria-controls="settings-panel-{section.id}"
          tabindex={activeSection === section.id ? 0 : -1}
          class="w-full rounded-[var(--radius-field)] text-left px-3 py-1.5 text-[13px] cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40
            {activeSection === section.id
              ? 'bg-accent/10 text-fg font-medium'
              : 'text-fg-muted hover:text-fg hover:bg-surface-2/30'}"
        >
          {section.label}
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
        {:else if activeSection === 'providers'}
          <ProviderSettings />
        {:else if activeSection === 'editor'}
          <EditorSection />
        {:else if activeSection === 'network'}
          <NetworkSection />
          <div class="mt-10">
            <RemoteEndpointsSection />
          </div>
        {:else if activeSection === 'discussions'}
          <DiscussionsSettings />
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
</div>
