<script lang="ts">
  import PipelineRow from './PipelineRow.svelte';
  import PullsListBody from './PullsListBody.svelte';
  import { pullsListItems, type PullsListProjectError } from './pullsListItems';
  import type { PipelineRowView } from './pipelineRowView';

  // The Pipelines tab's list: each failed project under its own heading,
  // then one row per pipeline.

  interface Props {
    rows: readonly PipelineRowView[];
    projectErrors: readonly PullsListProjectError[];
    selectedKey: string | null;
    error: string | null;
    loading: boolean;
    retrying: boolean;
    shown: boolean;
    onOpen: (row: PipelineRowView) => void;
    onRetry: () => void;
    onVisible: (keys: string[]) => void;
  }

  let { rows, projectErrors, selectedKey, error, loading, retrying, shown, onOpen, onRetry, onVisible }: Props = $props();

  const items = $derived(pullsListItems(projectErrors, 'pipelines-project-group', [{ id: 'pipelines', heading: null, rows }]));
</script>

<PullsListBody testid="pipelines-list" {items} rowEstimatePx={52} {shown} {onVisible} {onRetry} {retrying}>
  {#snippet row(view: PipelineRowView)}
    <PipelineRow row={view} selected={selectedKey === view.key} {onOpen} />
  {/snippet}
  {#snippet after()}
    {#if error}
      <p class="px-4 py-6 text-xs text-error" data-testid="pipelines-error">{error}</p>
    {:else if loading && rows.length === 0}
      <p class="px-4 py-6 text-xs text-fg-muted" data-testid="pipelines-loading">Reading pipelines…</p>
    {:else if rows.length === 0 && projectErrors.length === 0}
      <div class="flex flex-col items-center gap-1 px-6 py-10 text-center" data-testid="pipelines-empty">
        <p class="text-sm text-fg-muted">No pipelines match.</p>
      </div>
    {/if}
  {/snippet}
</PullsListBody>
