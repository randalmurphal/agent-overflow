<script lang="ts">
  import GitBranch from '@lucide/svelte/icons/git-branch';
  import GitPullRequestArrow from '@lucide/svelte/icons/git-pull-request-arrow';
  import Tag from '@lucide/svelte/icons/tag';
  import Icon from '../primitives/Icon.svelte';
  import { ciStatusDotClass, ciStatusText, ciStatusTextClass } from '../../utils/ciStatus';
  import { pullNumberLabel } from './pullRowView';
  import type { PipelineRowView } from './pipelineRowView';

  // One pipeline (GitLab) or workflow run (GitHub) in the Pipelines tab:
  // status dot and id, the ref it ran on with its kind glyph (branch, tag,
  // merge request) and the commit, stage dots once the row has been on
  // screen long enough to read them, and the age. Each color carries its
  // status in words for assistive tech. A row opens the stages / jobs /
  // log view beside the list.

  interface Props {
    row: PipelineRowView;
    selected: boolean;
    onOpen: (row: PipelineRowView, event: MouseEvent | KeyboardEvent) => void;
  }

  let { row, selected, onOpen }: Props = $props();

  const refIcon = $derived(row.refKind === 'tag' ? Tag : row.refKind === 'merge_request' ? GitPullRequestArrow : GitBranch);
  const refLabel = $derived(row.refKind === 'merge_request' && row.mrNumber > 0 ? pullNumberLabel(row.forge, row.mrNumber) : row.ref);
  const stageTitle = $derived((row.stages ?? []).map((stage) => `${stage.name}: ${ciStatusText(stage.status)}`).join('\n'));
</script>

<div
  class="relative flex items-stretch border-b border-border-subtle transition-colors {selected ? 'bg-accent/8' : 'hover:bg-surface-1/60'}"
  data-testid="pipeline-row"
  data-pipeline-key={row.key}
  data-selected={selected ? 'true' : undefined}
>
  {#if selected}
    <span class="absolute inset-y-0 left-0 w-0.5 bg-accent" aria-hidden="true"></span>
  {/if}
  <button
    type="button"
    class="grid min-w-0 flex-1 grid-cols-[auto_minmax(0,1fr)_auto_auto] items-center gap-x-3 gap-y-0.5 px-3 py-2 text-left text-[0.75rem] compact:select-none compact:py-2.5
      focus:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40"
    aria-current={selected ? 'true' : undefined}
    onclick={(event) => onOpen(row, event)}
    onkeydown={(event) => {
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        onOpen(row, event);
      }
    }}
  >
    <span class="inline-flex items-center gap-1.5 font-mono text-[0.6875rem] {ciStatusTextClass(row.status)}" data-testid="pipeline-status">
      <span class="h-2 w-2 rounded-full {ciStatusDotClass(row.status)}" aria-hidden="true"></span>#{row.id}<span class="sr-only">&nbsp;{ciStatusText(row.status)}</span>
    </span>
    <span class="flex min-w-0 flex-col">
      <span class="flex min-w-0 items-center gap-1.5">
        <span class="inline-flex shrink-0 items-center text-fg-subtle"><Icon icon={refIcon} size={11} strokeWidth={1.75} /></span>
        <span class="truncate font-medium text-fg">{refLabel}</span>
        {#if row.name}
          <span class="truncate text-fg-muted">· {row.name}</span>
        {/if}
      </span>
      <span class="flex min-w-0 items-center gap-1.5 text-[0.6875rem] text-fg-muted">
        <span class="shrink-0 font-mono">{row.shortSHA}</span>
        <span class="truncate">{row.title}</span>
        <span class="shrink-0 text-fg-subtle">{row.projectName}</span>
      </span>
    </span>
    <span class="flex items-center gap-[3px]" title={stageTitle} data-testid="pipeline-stages">
      {#each row.stages ?? [] as stage, index (`${index}:${stage.name}`)}
        <span class="h-2 w-2 rounded-full {ciStatusDotClass(stage.status)}" role="img" aria-label="{stage.name}: {ciStatusText(stage.status)}"></span>
      {/each}
    </span>
    <span class="shrink-0 text-[0.6875rem] text-fg-muted">{row.age}</span>
  </button>
</div>
