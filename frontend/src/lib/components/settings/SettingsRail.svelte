<script lang="ts">
  // The Settings nav rail: a search box over the grouped page tabs.
  //
  // With a query, the tabs give way to a result list over the field index
  // (`fields.ts`); picking a result opens its page and asks the view to
  // reveal the field. The query is kept after a pick so the user can hop
  // between hits; Esc or the clear button brings the tabs back. Esc with a
  // query stops here so the `settings.close` chord does not also close the
  // overlay — the first Esc clears, the second closes.
  //
  // A `tablist` only admits `tab` children, so each cluster wraps in a
  // `presentation` div (which drops out of the a11y tree, leaving the tabs as
  // direct children) and the group label is decorative: folding it into a
  // tab's accessible name would rename every tab.

  import Search from '@lucide/svelte/icons/search';
  import X from '@lucide/svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import UpdateBadge from '../shared/UpdateBadge.svelte';
  import { hasPendingUpdate } from '../../stores/updates.svelte';
  import {
    SETTINGS_SECTION_GROUPS,
    SETTINGS_SECTION_IDS,
    type SettingsSection,
  } from './sections';
  import {
    searchHitKey,
    searchSettings,
    type SettingsSearchHit,
  } from './settingsSearch';

  let {
    activeSection,
    onSelectSection,
    onSelectHit,
  }: {
    activeSection: SettingsSection;
    onSelectSection: (section: SettingsSection) => void;
    onSelectHit: (hit: SettingsSearchHit) => void;
  } = $props();

  let query = $state('');
  let hits = $derived(searchSettings(query));
  let searching = $derived(query.trim().length > 0);
  let highlighted = $state(0);
  let searchEl = $state<HTMLInputElement | null>(null);

  $effect(() => {
    // A new result list starts at the top; clamping keeps arrow-key state
    // valid when the list shrinks under it.
    if (highlighted >= hits.length) highlighted = 0;
  });

  function clearQuery(): void {
    query = '';
    highlighted = 0;
    searchEl?.focus();
  }

  function pick(hit: SettingsSearchHit): void {
    onSelectHit(hit);
  }

  function handleSearchKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      if (!searching) return;
      e.preventDefault();
      e.stopPropagation();
      clearQuery();
      return;
    }
    if (!searching) return;
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      highlighted = (highlighted + 1) % Math.max(hits.length, 1);
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      highlighted = (highlighted - 1 + hits.length) % Math.max(hits.length, 1);
    } else if (e.key === 'Enter') {
      e.preventDefault();
      const hit = hits[highlighted];
      if (hit) pick(hit);
    }
  }

  function handleTabKeydown(e: KeyboardEvent): void {
    const ids = SETTINGS_SECTION_IDS;
    const idx = ids.indexOf(activeSection);
    let next: SettingsSection | null = null;
    if (e.key === 'ArrowDown' || e.key === 'ArrowRight') {
      next = ids[(idx + 1) % ids.length];
    } else if (e.key === 'ArrowUp' || e.key === 'ArrowLeft') {
      next = ids[(idx - 1 + ids.length) % ids.length];
    } else if (e.key === 'Home') {
      next = ids[0];
    } else if (e.key === 'End') {
      next = ids[ids.length - 1];
    }
    if (next === null) return;
    e.preventDefault();
    onSelectSection(next);
    const target = next;
    requestAnimationFrame(() => {
      document.getElementById(`settings-tab-${target}`)?.focus();
    });
  }
</script>

<div class="flex w-56 shrink-0 flex-col border-r border-border-subtle">
  <div class="px-3 pt-3 pb-2">
    <div class="relative">
      <span
        class="pointer-events-none absolute left-2.5 top-1/2 flex -translate-y-1/2 items-center text-fg-hint"
        aria-hidden="true"
      >
        <Icon icon={Search} size={13} strokeWidth={2} class="opacity-70" />
      </span>
      <input
        bind:this={searchEl}
        bind:value={query}
        type="search"
        placeholder="Search settings"
        aria-label="Search settings"
        data-testid="settings-search"
        data-autofocus
        autocomplete="off"
        spellcheck="false"
        onkeydown={handleSearchKeydown}
        class="w-full rounded-[var(--radius-control)] border border-border-subtle bg-surface-0/60 py-1.5 pl-8 pr-7 text-[0.75rem] text-fg placeholder:text-fg-hint transition-colors focus:border-border focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
      />
      {#if searching}
        <button
          type="button"
          onclick={clearQuery}
          aria-label="Clear search"
          data-testid="settings-search-clear"
          class="absolute right-1.5 top-1/2 flex h-5 w-5 -translate-y-1/2 cursor-pointer items-center justify-center rounded-[var(--radius-field)] text-fg-subtle transition-colors hover:bg-surface-2/40 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
        >
          <Icon icon={X} size={12} strokeWidth={2.5} class="opacity-90" />
        </button>
      {/if}
    </div>
  </div>

  {#if searching}
    <div
      class="flex min-h-0 flex-1 flex-col gap-0.5 overflow-y-auto px-3 pb-4"
      role="listbox"
      aria-label="Search results"
      data-testid="settings-search-results"
    >
      {#if hits.length === 0}
        <p class="px-3 py-2 text-[0.75rem] text-fg-hint">No settings match.</p>
      {:else}
        {#each hits as hit, i (searchHitKey(hit))}
          {@const label = hit.kind === 'field' ? hit.field.label : hit.page.label}
          {@const crumb =
            hit.kind === 'field'
              ? hit.field.heading && hit.field.heading !== hit.field.label
                ? `${hit.page.label} › ${hit.field.heading}`
                : hit.page.label
              : hit.page.group}
          <button
            type="button"
            role="option"
            aria-selected={i === highlighted}
            data-testid="settings-search-hit"
            onclick={() => pick(hit)}
            onmouseenter={() => (highlighted = i)}
            class="w-full cursor-pointer rounded-[var(--radius-field)] px-3 py-1.5 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40
              {i === highlighted ? 'bg-accent/10 text-fg' : 'text-fg-muted hover:text-fg'}"
          >
            <span class="block text-[0.8125rem] font-medium leading-snug">{label}</span>
            <span class="block truncate text-[0.6875rem] text-fg-hint">{crumb}</span>
          </button>
        {/each}
      {/if}
    </div>
  {:else}
    <div
      class="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-3 pt-1 pb-4"
      role="tablist"
      aria-label="Settings Sections"
      aria-orientation="vertical"
    >
      {#each SETTINGS_SECTION_GROUPS as group (group.label)}
        <div role="presentation" class="flex flex-col gap-0.5">
          <p
            class="px-1.5 pb-1 text-[0.6875rem] font-semibold uppercase tracking-[0.18em] text-fg"
            aria-hidden="true"
          >
            {group.label}
          </p>
          {#each group.sections as section (section.id)}
            <button
              id="settings-tab-{section.id}"
              onclick={() => onSelectSection(section.id)}
              onkeydown={handleTabKeydown}
              role="tab"
              aria-selected={activeSection === section.id}
              aria-controls="settings-panel-{section.id}"
              tabindex={activeSection === section.id ? 0 : -1}
              class="w-full cursor-pointer rounded-[var(--radius-field)] px-3 py-1 text-left text-[0.8125rem] transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40
                {activeSection === section.id
                  ? 'bg-accent/10 font-medium text-fg'
                  : 'text-fg-muted hover:bg-surface-2/30 hover:text-fg'}"
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
  {/if}
</div>
