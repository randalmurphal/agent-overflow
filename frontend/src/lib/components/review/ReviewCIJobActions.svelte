<script lang="ts">
  import CircleStop from '@lucide/svelte/icons/circle-stop';
  import Download from '@lucide/svelte/icons/download';
  import Play from '@lucide/svelte/icons/play';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
  import type { Component } from 'svelte';
  import type { LucideProps } from '@lucide/svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import { CI_ACTION_LABEL, CI_ACTION_PENDING_LABEL, type CIJobActionKind } from '../../stores/ciActions.svelte';
  import type { CIActionsView } from '../../stores/ciSubjectActions.svelte';
  import type { CIJob } from '../../types/models';

  // A job's actions as icon buttons: the ones its CI read's flags show
  // (Run, Retry, Cancel, Download artifacts), and nothing else. The log
  // view's header and a job's row in the stages list both render it. The
  // pressed one stands pending until the forge's answer is on screen; the
  // others wait with the reason as their hover text, since one action
  // runs on a pipeline at a time.

  interface Props {
    actions: CIActionsView;
    job: CIJob;
    /** Test id prefix: `<prefix>-retry_job`, `<prefix>-artifacts`. */
    testPrefix: string;
  }

  let { actions, job, testPrefix }: Props = $props();

  const ICON: Record<CIJobActionKind, Component<LucideProps>> = {
    play_job: Play,
    retry_job: RotateCcw,
    cancel_job: CircleStop,
  };

  const kinds = $derived(actions.jobActions(job));
  const artifacts = $derived(actions.jobHasArtifacts(job));
  const busy = $derived(actions.pending !== null);
</script>

{#each kinds as kind (kind)}
  {@const pending = actions.isPending(kind, job)}
  <ReviewIconButton
    icon={ICON[kind]}
    label={CI_ACTION_LABEL[kind]}
    {pending}
    disabled={busy && !pending}
    disabledLabel={pending ? CI_ACTION_PENDING_LABEL[kind] : 'Another action on this pipeline is still running.'}
    onclick={() => { void actions.run(kind, job); }}
    testid="{testPrefix}-{kind}"
  />
{/each}
{#if artifacts}
  {@const downloading = actions.artifactsDownloading(job)}
  <ReviewIconButton
    icon={Download}
    label="Download artifacts"
    pending={downloading}
    disabledLabel="Downloading the artifacts…"
    onclick={() => { void actions.saveArtifacts(job); }}
    testid="{testPrefix}-artifacts"
  />
{/if}
