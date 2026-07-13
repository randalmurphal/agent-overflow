<script lang="ts">
  import type { WorkflowArtifact } from '../../../../bindings/agent-overflow/models';

  interface Props {
    values: Record<string, unknown>;
    artifacts: WorkflowArtifact[];
    viewOnly: boolean;
    onOpenArtifact: (path: string) => void;
  }

  let { values, artifacts, viewOnly, onOpenArtifact }: Props = $props();
  let entries = $derived(Object.entries(values).sort(([left], [right]) => left.localeCompare(right)));

  function formatValue(value: unknown): string {
    if (typeof value === 'string') return value;
    if (value === null || value === undefined) return '—';
    if (typeof value === 'object') return JSON.stringify(value);
    return String(value);
  }
</script>

{#if entries.length > 0 || artifacts.length > 0}
  <section class="space-y-1.5" data-testid="wf-outputs">
    <h3 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">Outputs</h3>
    {#if entries.length > 0}
      <dl class="divide-y divide-border-subtle rounded-md border border-border-subtle" data-testid="wf-output-values">
        {#each entries as [name, value] (name)}
          <div class="grid grid-cols-[minmax(0,1fr)_minmax(0,2fr)] gap-3 px-2.5 py-2 text-xs">
            <dt class="truncate font-medium text-fg-muted">{name}</dt>
            <dd class="break-words text-right">{formatValue(value)}</dd>
          </div>
        {/each}
      </dl>
    {/if}
    {#each artifacts as artifact (artifact.path)}
      <button class="flex w-full items-center gap-2 rounded-md border border-border-subtle px-2.5 py-2 text-left text-xs hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-50" onclick={() => onOpenArtifact(artifact.path)} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-output-file">
        <span>↗</span><span class="min-w-0 flex-1 truncate">{artifact.name}</span><span class="text-fg-muted">{artifact.size} bytes</span>
      </button>
    {/each}
  </section>
{/if}
