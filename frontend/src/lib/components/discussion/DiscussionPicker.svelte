<script lang="ts">
  import type { DiscussionDefinition } from '../../types/discussion';

  let {
    discussions,
    selectedName,
    onSelect,
    loading = false,
    emptyLabel = 'No discussions available',
  }: {
    discussions: DiscussionDefinition[];
    selectedName: string | null;
    onSelect: (def: DiscussionDefinition) => void;
    loading?: boolean;
    emptyLabel?: string;
  } = $props();

  function scopeLabel(def: DiscussionDefinition): string {
    return def.scope === 'project' ? 'Project' : 'Global';
  }

  function scopeClasses(def: DiscussionDefinition): string {
    return def.scope === 'project'
      ? 'bg-provider-codex/15 text-provider-codex border-provider-codex/25'
      : 'bg-accent/15 text-accent border-accent/25';
  }

  function handleKeydown(e: KeyboardEvent, def: DiscussionDefinition): void {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      onSelect(def);
    }
  }
</script>

<div class="space-y-1" role="listbox" aria-label="Discussions" aria-busy={loading}>
  {#if loading}
    <div class="rounded-xl border border-border/50 bg-surface-1/60 px-4 py-3 text-xs text-text-secondary">
      Loading discussions...
    </div>
  {:else if discussions.length === 0}
    <div class="rounded-xl border border-border/50 bg-surface-1/60 px-4 py-3 text-xs text-text-secondary">
      {emptyLabel}
    </div>
  {:else}
    {#each discussions as def (def.id || `${def.scope}::${def.name}`)}
      <button
        type="button"
        role="option"
        aria-selected={selectedName === def.name}
        onclick={() => onSelect(def)}
        onkeydown={(e) => handleKeydown(e, def)}
        class="w-full flex items-start gap-3 rounded-xl border px-3 py-2.5 text-left transition-colors cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50
          {selectedName === def.name
            ? 'border-accent/60 bg-accent/10'
            : 'border-border/60 bg-surface-1/60 hover:border-text-secondary/40 hover:bg-surface-2/40'}"
      >
        <span class="mt-0.5 shrink-0 text-[10px] font-semibold uppercase tracking-[0.14em] rounded-md border px-1.5 py-0.5 {scopeClasses(def)}">
          {scopeLabel(def)}
        </span>
        <span class="min-w-0 flex-1">
          <span class="block text-sm font-medium text-text-primary truncate">{def.name}</span>
          {#if def.description}
            <span class="mt-0.5 block text-xs text-text-secondary line-clamp-2">{def.description}</span>
          {/if}
          <span class="mt-1 block text-[11px] text-text-secondary/70">
            {def.participants.length} participants · {def.settings.maxTurns} turns
          </span>
        </span>
      </button>
    {/each}
  {/if}
</div>
