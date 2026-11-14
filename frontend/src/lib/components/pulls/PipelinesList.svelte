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
    /** The list find's query; non-empty while it narrows the rows. */
    query: string;
    error: string | null;
    loading: boolean;
    retrying: boolean;
    shown: boolean;
    onOpen: (row: PipelineRowView) => void;
    onRetry: () => void;
    onVisible: (keys: string[]) => void;
  }

  let { rows, projectErrors, selectedKey, query, error, loading, retrying, shown, onOpen, onRetry, onVisible }: Props = $props();

  let body: PullsListBody<PipelineRowView> | undefined = $state(undefined);

  export function focusFirstRow(): Promise<boolean> {
    return body?.focusFirstRow() ?? Promise.resolve(false);
  }

  export function scroller(): HTMLElement | null {
    return body?.scroller() ?? null;
  }

  const items = $derived(pullsListItems(projectErrors, 'pipelines-project-group', [{ id: 'pipelines', heading: null, rows }]));
</script>

<PullsListBody bind:this={body} testid="pipelines-list" {items} rowEstimatePx={52} {shown} {onVisible} {onRetry} {retrying}>
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
        {#if query.trim()}
          <p class="text-sm text-fg-muted">No listed pipelines match "{query.trim()}".</p>
          <p class="text-xs text-fg-subtle">The tab lists each project's most recent pipelines.</p>
        {:else}
          <p class="text-sm text-fg-muted">No pipelines match.</p>
        {/if}
      </div>
    {/if}
  {/snippet}
</PullsListBody>
