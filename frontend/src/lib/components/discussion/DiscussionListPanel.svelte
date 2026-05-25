<script lang="ts">
  import type { DiscussionDefinition } from '../../types/discussion';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import Button from '../primitives/Button.svelte';

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
    <MicroLabel as="p">Definitions</MicroLabel>
    <Button variant="secondary" size="xs" onclick={onNew}>
      {#snippet children()}+ New{/snippet}
    </Button>
  </div>

  <div class="flex-1 min-h-0 overflow-y-auto space-y-1 pr-1">
    {#if loading}
      <div class="rounded-[var(--radius-control)] border border-border-subtle bg-card/30 px-3 py-2 text-[0.75rem] text-fg-muted">
        Loading…
      </div>
    {:else if discussions.length === 0}
      <div class="rounded-[var(--radius-control)] border border-border-subtle bg-card/30 px-3 py-3 text-[0.75rem] text-fg-muted">
        No discussions yet. Create one to enable multi-agent deliberation on a thread.
      </div>
    {:else}
      {#each discussions as def (keyFor(def))}
        {@const active = selectedKey === keyFor(def)}
        <button
          type="button"
          onclick={() => onSelect(def)}
          class="w-full flex items-start gap-2 rounded-[var(--radius-control)] border px-3 py-2 text-left transition-colors cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40
            {active
              ? 'border-accent/50 bg-accent/10'
              : 'border-border-subtle bg-card/30 hover:border-border hover:bg-surface-2/30'}"
        >
          <span class="mt-0.5 shrink-0 rounded-[var(--radius-field)] border px-1.5 py-0.5 text-[0.625rem] font-semibold uppercase tracking-[0.14em]
            {def.scope === 'project' ? 'border-provider-codex/25 bg-provider-codex/10 text-provider-codex' : 'border-accent/25 bg-accent/10 text-accent'}"
          >
            {scopeLabel(def)}
          </span>
          <span class="min-w-0 flex-1">
            <span class="block text-[0.8125rem] font-medium text-fg truncate">{def.name}</span>
            <span class="mt-0.5 block text-[0.625rem] text-fg-hint tabular-nums">
              {def.participants.length} participants
            </span>
          </span>
        </button>
      {/each}
    {/if}
  </div>
</div>
