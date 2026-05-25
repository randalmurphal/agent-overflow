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
      ? 'bg-provider-codex/10 text-provider-codex border-provider-codex/25'
      : 'bg-accent/10 text-accent border-accent/25';
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
    <div class="rounded-[var(--radius-control)] border border-border-subtle bg-card/30 px-4 py-3 text-[0.75rem] text-fg-muted">
      Loading discussions…
    </div>
  {:else if discussions.length === 0}
    <div class="rounded-[var(--radius-control)] border border-border-subtle bg-card/30 px-4 py-3 text-[0.75rem] text-fg-muted">
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
        class="w-full flex items-start gap-3 rounded-[var(--radius-control)] border px-3 py-2 text-left transition-colors cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40
          {selectedName === def.name
            ? 'border-accent/50 bg-accent/10'
            : 'border-border-subtle bg-card/30 hover:border-border hover:bg-surface-2/30'}"
      >
        <span class="mt-0.5 shrink-0 text-[0.625rem] font-semibold uppercase tracking-[0.14em] rounded-[var(--radius-field)] border px-1.5 py-0.5 {scopeClasses(def)}">
          {scopeLabel(def)}
        </span>
        <span class="min-w-0 flex-1">
          <span class="block text-[0.8125rem] font-medium text-fg truncate">{def.name}</span>
          {#if def.description}
            <span class="mt-0.5 block text-[0.75rem] text-fg-muted line-clamp-2">{def.description}</span>
          {/if}
          <span class="mt-1 block text-[0.625rem] text-fg-hint tabular-nums">
            {def.participants.length} participants · {def.settings.maxTurns} turns
          </span>
        </span>
      </button>
    {/each}
  {/if}
</div>
