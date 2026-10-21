<script lang="ts">
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import ExternalLink from '@lucide/svelte/icons/external-link';
  import GitBranch from '@lucide/svelte/icons/git-branch';
  import GitPullRequestArrow from '@lucide/svelte/icons/git-pull-request-arrow';
  import Tag from '@lucide/svelte/icons/tag';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import RenderBoundary from '../shared/RenderBoundary.svelte';
  import ReviewCIChips from '../review/ReviewCIChips.svelte';
  import ReviewCILogView from '../review/ReviewCILogView.svelte';
  import { OpenExternalURL } from '../../stores/bindings';
  import type { CISubjectState } from '../../stores/ciSubject.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { ciStatusTextClass } from '../../utils/ciStatus';
  import { errString } from '../../utils/errors';
  import { pullNumberLabel } from './pullRowView';
  import type { PipelineRowView } from './pipelineRowView';

  // The Pipelines tab's detail beside the list: the pipeline's title line
  // (its ref, id, status, commit and age, with the forge link), its stage
  // chips, and the job log a chip opens, all from the selected pipeline's
  // CI subject state. A repository pipeline has no chat to send to, so the
  // log view offers no send. Compact shows it full width; Back returns to
  // the list.

  interface Props {
    row: PipelineRowView;
    ci: CISubjectState;
    /** Back to the list; compact only. */
    onBack?: () => void;
  }

  let { row, ci, onBack }: Props = $props();

  const refIcon = $derived(row.refKind === 'tag' ? Tag : row.refKind === 'merge_request' ? GitPullRequestArrow : GitBranch);
  const refLabel = $derived(row.refKind === 'merge_request' && row.mrNumber > 0 ? pullNumberLabel(row.forge, row.mrNumber) : row.ref);
  const status = $derived(ci.pipeline?.status ?? row.status);
  const url = $derived(ci.pipeline?.url || row.url);

  async function openOnForge(): Promise<void> {
    if (!url) return;
    try {
      await OpenExternalURL(url);
    } catch (err) {
      addErrorToast(`Could not open the pipeline: ${errString(err)}`, err);
    }
  }
</script>

<RenderBoundary label="The pipeline view" testId="pipeline-view-render-error">
  <section class="flex min-h-0 min-w-0 flex-1 flex-col" data-testid="pipeline-view" data-pipeline-key={row.key}>
    <header class="flex min-w-0 items-center gap-2 border-b border-border-subtle px-3 py-2 text-[0.75rem]">
      {#if onBack}
        <IconButton label="Back to the list" size="sm" onClick={onBack} testId="pipeline-view-back">
          <Icon icon={ArrowLeft} size={14} strokeWidth={1.75} />
        </IconButton>
      {/if}
      <span class="inline-flex shrink-0 items-center text-fg-subtle"><Icon icon={refIcon} size={12} strokeWidth={1.75} /></span>
      <span class="min-w-0 truncate font-medium text-fg" title={row.ref}>{refLabel}</span>
      <span class="shrink-0 font-mono text-[0.6875rem] text-fg-muted">#{row.id}</span>
      <span class="shrink-0 text-[0.6875rem] {ciStatusTextClass(status)}" data-testid="pipeline-view-status">{status}</span>
      <span class="shrink-0 font-mono text-[0.6875rem] text-fg-muted">{row.shortSHA}</span>
      {#if row.title}
        <span class="min-w-0 truncate text-fg-muted">{row.title}</span>
      {/if}
      <span class="ml-auto shrink-0 text-[0.6875rem] text-fg-subtle">{row.age}</span>
      {#if url}
        <IconButton label="Open on forge" size="sm" onClick={() => { void openOnForge(); }} testId="pipeline-view-open-forge">
          <Icon icon={ExternalLink} size={13} strokeWidth={1.75} />
        </IconButton>
      {/if}
    </header>
    <div class="flex min-w-0 flex-wrap items-center gap-1 border-b border-border-subtle px-3 py-1.5" data-testid="pipeline-view-chips">
      <ReviewCIChips
        pipeline={ci.pipeline}
        loading={ci.loading}
        error={ci.error}
        failure={ci.failure}
        forge={row.forge}
        refreshing={ci.refreshing}
        onOpenJob={(stageName, job) => ci.openJobLog(stageName, job)}
        onRefresh={() => { void ci.refresh(); }}
      />
    </div>
    {#if ci.logView}
      <ReviewCILogView
        view={ci.logView}
        log={ci.log}
        fetchLines={(from, count) => ci.readLines(from, count)}
        loading={ci.logLoading}
        error={ci.logError}
        failure={ci.logFailure}
        forge={row.forge}
        savedPath={ci.logSavedPath}
        openSections={ci.logOpenSections}
        onBack={() => ci.closeLogView()}
        onRefresh={() => ci.refreshLog()}
        onSave={() => { void ci.saveLog(); }}
        onToggleSection={(sectionKey) => ci.toggleSection(sectionKey)}
        onSetSectionsOpen={(sectionKeys, open) => ci.setSectionsOpen(sectionKeys, open)}
        onCopySection={(section) => ci.copySection(section)}
      />
    {:else}
      <div class="flex flex-1 items-center justify-center px-6" data-testid="pipeline-view-empty">
        <p class="text-sm text-fg-muted">Open a stage and pick a job to read its log.</p>
      </div>
    {/if}
  </section>
</RenderBoundary>
