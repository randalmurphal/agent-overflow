<script lang="ts">
  import GeneralSettings from './GeneralSettings.svelte';
  import ProviderSettings from './ProviderSettings.svelte';
  import ArchivedThreads from './ArchivedThreads.svelte';
  import DiscussionsSettings from './DiscussionsSettings.svelte';
  import KeybindingsSettings from './KeybindingsSettings.svelte';

  let { onClose }: { onClose: () => void } = $props();

  type Section = 'general' | 'providers' | 'discussions' | 'keybindings' | 'archived';
  let activeSection: Section = $state('general');

  const sections: Array<{ id: Section; label: string }> = [
    { id: 'general', label: 'General' },
    { id: 'providers', label: 'Providers' },
    { id: 'discussions', label: 'Discussions' },
    { id: 'keybindings', label: 'Keybindings' },
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
  <div class="flex items-center gap-2 border-b border-border/70 bg-surface-1/75 px-5 py-3 shadow-[0_10px_30px_-28px_rgba(0,0,0,0.45)] backdrop-blur-sm shrink-0">
    <div>
      <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Preferences</p>
      <h2 class="mt-1 text-sm font-semibold text-text-primary">Settings</h2>
    </div>
    <button
      onclick={onClose}
      class="ml-auto text-text-secondary hover:text-text-primary cursor-pointer p-1 rounded focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      aria-label="Close settings"
    >
      <svg class="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
        <path d="M18 6L6 18M6 6l12 12" />
      </svg>
    </button>
  </div>

  <div class="flex flex-1 min-h-0 gap-5 p-5">
    <div class="w-48 shrink-0 rounded-2xl border border-border/70 bg-surface-1/75 p-2 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm" role="tablist" aria-label="Settings sections">
      {#each sections as section}
        <button
          id="settings-tab-{section.id}"
          onclick={() => activeSection = section.id}
          onkeydown={handleTabKeydown}
          role="tab"
          aria-selected={activeSection === section.id}
          aria-controls="settings-panel-{section.id}"
          tabindex={activeSection === section.id ? 0 : -1}
          class="w-full rounded-xl text-left px-4 py-2.5 text-sm cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/50
            {activeSection === section.id
              ? 'bg-accent/12 text-accent font-medium shadow-[inset_0_1px_0_rgba(255,255,255,0.04)]'
              : 'text-text-secondary hover:text-text-primary hover:bg-surface-2/50'}"
        >
          {section.label}
        </button>
      {/each}
    </div>

    <div class="flex-1 overflow-y-auto rounded-[28px] border border-border/60 bg-surface-0/45 p-1 shadow-[inset_0_1px_0_rgba(255,255,255,0.03)]" role="tabpanel" id="settings-panel-{activeSection}" aria-labelledby="settings-tab-{activeSection}">
      <div
        class="min-h-full rounded-[24px] p-6"
        style="background: linear-gradient(180deg, color-mix(in oklab, var(--surface-1) 82%, transparent), color-mix(in oklab, var(--surface-0) 92%, transparent));"
      >
        <div class="mx-auto max-w-4xl">
          {#if activeSection === 'general'}
            <GeneralSettings />
          {:else if activeSection === 'providers'}
            <ProviderSettings />
          {:else if activeSection === 'discussions'}
            <DiscussionsSettings />
          {:else if activeSection === 'keybindings'}
            <KeybindingsSettings />
          {:else if activeSection === 'archived'}
            <ArchivedThreads />
          {/if}
        </div>
      </div>
    </div>
  </div>
</div>
