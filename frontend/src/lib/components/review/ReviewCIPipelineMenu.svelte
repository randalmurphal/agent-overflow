<script lang="ts">
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { CI_ACTION_LABEL, CI_ACTION_PENDING_LABEL, type CIPipelineActionKind } from '../../stores/ciActions.svelte';
  import type { CIActionsView } from '../../stores/ciSubjectActions.svelte';

  // The CI header's one menu of pipeline actions: Retry failed jobs,
  // Cancel pipeline and, on GitHub, Download artifacts, each only when the
  // read's flags show it. When a read behind the flags failed, a muted,
  // non-interactive row says why (actionsUnavailable), and the trigger
  // shows for it alone. With nothing to offer or say there is no trigger
  // at all. While a pipeline action is pending, the trigger shows it and
  // its rows wait with the reason.

  interface Props {
    actions: CIActionsView;
  }

  let { actions }: Props = $props();

  let open = $state(false);
  let triggerEl: HTMLButtonElement | undefined = $state();

  const kinds = $derived(actions.pipelineActions);
  const artifacts = $derived(actions.pipelineHasArtifacts);
  const downloading = $derived(actions.artifactsDownloading(null));
  // A pipeline action pending (a job's shows on the job).
  const pendingKind = $derived<CIPipelineActionKind | null>(
    actions.pending !== null && actions.pending.jobId === null ? (actions.pending.kind as CIPipelineActionKind) : null,
  );
  const busy = $derived(actions.pending !== null);
  const pendingLabel = $derived(pendingKind ? CI_ACTION_PENDING_LABEL[pendingKind] : downloading ? 'Downloading the artifacts…' : null);
  const unavailable = $derived(actions.actionsUnavailable);
  const shown = $derived(kinds.length > 0 || artifacts || pendingLabel !== null || unavailable !== null);

  function close(): void {
    open = false;
  }

  function pick(run: () => void): void {
    close();
    run();
  }
</script>

{#if shown}
  <button
    bind:this={triggerEl}
    type="button"
    class="inline-flex size-6 items-center justify-center rounded text-fg-subtle hover:bg-surface-2 hover:text-fg compact:size-8 {open ? 'bg-surface-2 text-fg' : ''}"
    aria-label={pendingLabel ?? 'Pipeline actions'}
    title={pendingLabel ?? 'Pipeline actions'}
    aria-haspopup="menu"
    aria-expanded={open}
    data-testid="review-ci-pipeline-actions"
    data-pending={pendingLabel !== null || undefined}
    onclick={() => { open = !open; }}
  >
    {#if pendingLabel !== null}
      <SteppedSpinner size={12} />
    {:else}
      <Icon icon={Ellipsis} size={12} />
    {/if}
  </button>
  <Popover anchor={triggerEl} open={open} onClose={close} placement="bottom-end" role="none">
    <Menu ariaLabel="Pipeline actions" onClose={close} minWidthClass="min-w-[180px]">
      {#each kinds as kind (kind)}
        <MenuItem
          label={pendingKind === kind ? CI_ACTION_PENDING_LABEL[kind] : CI_ACTION_LABEL[kind]}
          disabled={busy}
          title={busy ? (pendingKind === kind ? CI_ACTION_PENDING_LABEL[kind] : 'Another action on this pipeline is still running.') : undefined}
          variant={kind === 'cancel_pipeline' ? 'danger' : 'default'}
          testId="review-ci-pipeline-{kind}"
          onSelect={() => pick(() => { void actions.run(kind, null); })}
        />
      {/each}
      {#if artifacts}
        <MenuItem
          label={downloading ? 'Downloading the artifacts…' : 'Download artifacts'}
          disabled={downloading}
          title={downloading ? 'Downloading the artifacts…' : undefined}
          testId="review-ci-pipeline-artifacts"
          onSelect={() => pick(() => { void actions.saveArtifacts(null); })}
        />
      {/if}
      {#if unavailable !== null}
        <div
          role="presentation"
          class="max-w-[280px] px-3 py-1.5 text-xs text-text-secondary select-none"
          data-testid="review-ci-pipeline-unavailable"
        >
          {unavailable}
        </div>
      {/if}
    </Menu>
  </Popover>
{/if}
