<script lang="ts">
  import GeneralSettings from './GeneralSettings.svelte';
  import ProviderSettings from './ProviderSettings.svelte';
  import ArchivedThreads from './ArchivedThreads.svelte';

  let { onClose }: { onClose: () => void } = $props();

  type Section = 'general' | 'providers' | 'archived';
  let activeSection: Section = $state('general');

  const sections: Array<{ id: Section; label: string }> = [
    { id: 'general', label: 'General' },
    { id: 'providers', label: 'Providers' },
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

<div class="flex flex-col h-full">
  <div class="border-b border-border bg-surface-1 px-4 py-2.5 flex items-center gap-2 shrink-0">
    <h2 class="text-sm font-medium text-text-primary">Settings</h2>
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

  <div class="flex flex-1 min-h-0">
    <div class="w-40 shrink-0 border-r border-border bg-surface-0 py-2" role="tablist" aria-label="Settings sections">
      {#each sections as section}
        <button
          id="settings-tab-{section.id}"
          onclick={() => activeSection = section.id}
          onkeydown={handleTabKeydown}
          role="tab"
          aria-selected={activeSection === section.id}
          aria-controls="settings-panel-{section.id}"
          tabindex={activeSection === section.id ? 0 : -1}
          class="w-full text-left px-4 py-1.5 text-sm cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/50
            {activeSection === section.id
              ? 'text-accent font-medium bg-accent/10'
              : 'text-text-secondary hover:text-text-primary hover:bg-surface-2/50'}"
        >
          {section.label}
        </button>
      {/each}
    </div>

    <div class="flex-1 overflow-y-auto p-6" role="tabpanel" id="settings-panel-{activeSection}" aria-labelledby="settings-tab-{activeSection}">
      {#if activeSection === 'general'}
        <GeneralSettings />
      {:else if activeSection === 'providers'}
        <ProviderSettings />
      {:else if activeSection === 'archived'}
        <ArchivedThreads />
      {/if}
    </div>
  </div>
</div>
