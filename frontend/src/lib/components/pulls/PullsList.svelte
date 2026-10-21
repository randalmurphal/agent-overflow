<script lang="ts">
  import PullRow from './PullRow.svelte';
  import PullsListBody from './PullsListBody.svelte';
  import { pullsListItems, type PullsListProjectError } from './pullsListItems';
  import type { PullRowView } from './pullRowView';
  import { pullGroupLabel } from '../../utils/pullsAttention';
  import type { PullsFilter } from '../../stores/pullsPageFilters.svelte';

  // The Merge requests tab's list: each failed project under its own
  // heading, then the Needs you and Waiting on others groups.

  interface Props {
    groups: readonly { group: string; rows: readonly PullRowView[] }[];
    projectErrors: readonly PullsListProjectError[];
    selectedKey: string | null;
    filter: PullsFilter;
    /** No project on the computer has a forge origin. */
    noProjects: boolean;
    error: string | null;
    loading: boolean;
    retrying: boolean;
    shown: boolean;
    onOpen: (row: PullRowView) => void;
    onMenu: (row: PullRowView, anchor: HTMLElement) => void;
    onRetry: () => void;
    onVisible: (keys: string[]) => void;
  }

  let { groups, projectErrors, selectedKey, filter, noProjects, error, loading, retrying, shown, onOpen, onMenu, onRetry, onVisible }: Props = $props();

  const items = $derived(
    pullsListItems(
      projectErrors,
      'pulls-project-group',
      groups.map((group) => ({
        id: group.group,
        heading: { label: pullGroupLabel(group.group), count: group.rows.length, testid: `pulls-group-${group.group}` },
        rows: group.rows,
      })),
    ),
  );
  const rowCount = $derived(groups.reduce((sum, group) => sum + group.rows.length, 0));

  const emptyText = $derived.by(() => {
    if (noProjects) return 'No project on this computer has a GitHub or GitLab origin.';
    switch (filter) {
      case 'needs_you':
        return 'Nothing needs you right now.';
      case 'mine':
        return 'No open merge requests of yours.';
      case 'review_requested':
        return 'No reviews requested from you.';
      default:
        return 'No open merge requests.';
    }
  });
</script>

<PullsListBody testid="pulls-list" {items} rowEstimatePx={76} {shown} {onVisible} {onRetry} {retrying}>
  {#snippet row(view: PullRowView)}
    <PullRow row={view} selected={selectedKey === view.key} {onOpen} {onMenu} />
  {/snippet}
  {#snippet after()}
    {#if error}
      <p class="px-4 py-6 text-xs text-error" data-testid="pulls-error">{error}</p>
    {:else if loading && rowCount === 0 && projectErrors.length === 0}
      <p class="px-4 py-6 text-xs text-fg-muted" data-testid="pulls-loading">Reading merge requests…</p>
    {:else if rowCount === 0 && projectErrors.length === 0}
      <div class="flex flex-col items-center gap-1 px-6 py-10 text-center" data-testid="pulls-empty">
        <p class="text-sm text-fg-muted">{emptyText}</p>
      </div>
    {/if}
  {/snippet}
</PullsListBody>
