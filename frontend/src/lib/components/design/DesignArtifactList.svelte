<script lang="ts">
  // DesignArtifactList — scrollable timeline of rendered artifacts for the
  // current thread. Clicking a row pins that artifact in the preview via
  // pane.setActiveArtifact.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { relativeTime } from '../../utils/format';

  let { pane }: { pane: ThreadPane } = $props();

  let sorted = $derived(
    [...pane.designArtifacts].sort((a, b) => b.createdAt - a.createdAt),
  );

  // The preview falls back to "latest" when activeArtifactId is null. For
  // highlighting the row, we mirror that fallback so the "current" row is
  // always visible.
  let activeId = $derived(
    pane.activeArtifactId
      ?? (sorted.length > 0 ? sorted[0].id : null),
  );

  function pickArtifact(id: string) {
    pane.setActiveArtifact(id);
  }
</script>

<aside class="w-64 shrink-0 border-l border-border bg-surface-1 flex flex-col min-h-0">
  <div class="px-3 py-2 border-b border-border shrink-0">
    <p class="text-[11px] font-semibold uppercase tracking-wider text-text-secondary/70">
      Artifact history
    </p>
    <p class="text-[10px] text-text-secondary/60 mt-0.5">
      {sorted.length} {sorted.length === 1 ? 'artifact' : 'artifacts'}
    </p>
  </div>
  <div class="flex-1 min-h-0 overflow-y-auto py-1 px-1">
    {#if sorted.length === 0}
      <p class="px-3 py-4 text-xs text-text-secondary/70">No artifacts yet.</p>
    {:else}
      {#each sorted as artifact (artifact.id)}
        {@const isActive = artifact.id === activeId}
        <button
          type="button"
          onclick={() => pickArtifact(artifact.id)}
          aria-pressed={isActive}
          class="w-full text-left px-2 py-1.5 rounded-md cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50
            {isActive
              ? 'bg-accent/15 text-text-primary'
              : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary'}"
        >
          <div class="flex items-center gap-1.5">
            <span class="text-[9px] px-1 py-0.5 rounded shrink-0
              {artifact.kind === 'option'
                ? 'bg-provider-codex/20 text-provider-codex'
                : 'bg-accent/20 text-accent'}" aria-hidden="true">
              {artifact.kind === 'option' ? 'OPT' : 'REN'}
            </span>
            <span class="text-xs truncate flex-1">{artifact.title}</span>
          </div>
          <div class="text-[10px] text-text-secondary/60 mt-0.5 ml-8">
            {relativeTime(artifact.createdAt, getSettings().timestampFormat)}
          </div>
        </button>
      {/each}
    {/if}
  </div>
</aside>
