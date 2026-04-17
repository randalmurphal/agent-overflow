<script lang="ts">
  import type { DiscussionDefinition } from '../../types/discussion';

  let {
    discussions,
    selectedKey,
    onSelect,
    onNew,
    loading = false,
  }: {
    discussions: DiscussionDefinition[];
    selectedKey: string | null;
    onSelect: (def: DiscussionDefinition) => void;
    onNew: () => void;
    loading?: boolean;
  } = $props();

  function keyFor(def: DiscussionDefinition): string {
    return `${def.scope}::${def.name}`;
  }

  function scopeLabel(def: DiscussionDefinition): string {
    return def.scope === 'project' ? 'Project' : 'Global';
  }
</script>

<div class="flex h-full min-h-0 flex-col">
  <div class="mb-3 flex items-center justify-between gap-2 shrink-0">
    <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Definitions</p>
    <button
      type="button"
      onclick={onNew}
      class="rounded-xl border border-border px-3 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      + New
    </button>
  </div>

  <div class="flex-1 min-h-0 overflow-y-auto space-y-1 pr-1">
    {#if loading}
      <div class="rounded-xl border border-border/50 bg-surface-1/60 px-3 py-2 text-xs text-text-secondary">
        Loading...
      </div>
    {:else if discussions.length === 0}
      <div class="rounded-xl border border-border/50 bg-surface-1/60 px-3 py-3 text-xs text-text-secondary">
        No discussions yet. Create one to enable multi-agent deliberation on a thread.
      </div>
    {:else}
      {#each discussions as def (keyFor(def))}
        {@const active = selectedKey === keyFor(def)}
        <button
          type="button"
          onclick={() => onSelect(def)}
          class="w-full flex items-start gap-2 rounded-xl border px-3 py-2 text-left transition-colors cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50
            {active
              ? 'border-accent/60 bg-accent/10'
              : 'border-border/60 bg-surface-1/60 hover:border-text-secondary/40 hover:bg-surface-2/40'}"
        >
          <span class="mt-0.5 shrink-0 rounded-md border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-[0.14em]
            {def.scope === 'project' ? 'border-provider-codex/25 bg-provider-codex/10 text-provider-codex' : 'border-accent/25 bg-accent/10 text-accent'}"
          >
            {scopeLabel(def)}
          </span>
          <span class="min-w-0 flex-1">
            <span class="block text-sm font-medium text-text-primary truncate">{def.name}</span>
            <span class="mt-0.5 block text-[11px] text-text-secondary/70">
              {def.participants.length} participants
            </span>
          </span>
        </button>
      {/each}
    {/if}
  </div>
</div>
