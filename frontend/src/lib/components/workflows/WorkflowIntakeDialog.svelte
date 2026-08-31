<script lang="ts">
  // New run (UI-SPEC §5.1). Project · Goal · Workflow · Base branch · the
  // workflow's own fields · step mode. The footer's primary is `Start`: rev 2
  // has no queue, so there is no position to show and nothing to predict.
  //
  // Definitions come from the catalog the overlay already loaded, so opening
  // this dialog costs no round trip; an invalid definition is greyed with its
  // first error rather than hidden, because "why can't I pick this?" is the
  // question a hidden row leaves behind.

  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import WorkflowSeedFields from './WorkflowSeedFields.svelte';
  import type { WorkflowDefinitionListing } from '../../types/workflow';
  import { WorkflowStartRun } from '../../stores/bindings';
  import { getProjectLabelText, getProjects } from '../../stores/projects.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { hasScope } from '../../transport/scopes';
  import { workflowChainSummary, workflowDefinitionMeta } from '../../stores/workflowData';
  import { compactWorkflowSeeds, workflowIntakeError, workflowSeedDefault } from '../../utils/workflowIntake';
  import { getWorkflowCatalog, refreshWorkflowRunsSoon } from '../../stores/workflowRuns.svelte';
  import { getWorkflowProjectFilter } from '../../stores/workflowsOverlay.svelte';

  interface Props {
    open: boolean;
    onClose: () => void;
  }
  let { open, onClose }: Props = $props();

  // Every control here drives the workflow engine, which is `threads:autonomy`.
  let ungranted = $derived(!hasScope('threads:autonomy'));
  let projects = $derived(getProjects());
  let projectId = $state('');
  let goal = $state('');
  let workflowId = $state('');
  let seeds = $state<Record<string, unknown>>({});
  let baseBranch = $state('');
  let baseBranchEdited = $state(false);
  let stepMode = $state(false);
  let submitting = $state(false);
  let wasOpen = false;
  let seededFor = '';

  let catalog = $derived(getWorkflowCatalog(projectId));
  let definitions = $derived(catalog?.workflows ?? []);
  let selected = $derived(definitions.find((definition) => definition.id === workflowId) ?? null);
  let browseRoot = $derived(projects.find((entry) => entry.project.id === projectId)?.project.path ?? '~');
  let validationError = $derived(workflowIntakeError({ projectId, goal, definition: selected, seeds }));

  $effect(() => {
    if (open === wasOpen) return;
    wasOpen = open;
    if (!open) return;
    // Open on the project the human is already looking at (§3.1 filter).
    projectId = getWorkflowProjectFilter() || projects[0]?.project.id || '';
    goal = '';
    workflowId = '';
    seeds = {};
    baseBranch = '';
    baseBranchEdited = false;
    stepMode = false;
    seededFor = '';
    submitting = false;
  });

  // Picking a workflow adopts its declared defaults and its base branch, until
  // the human types their own — an edited branch survives switching workflows.
  $effect(() => {
    const definition = selected;
    if (!definition) return;
    const key = `${projectId}\n${definition.id}`;
    if (seededFor === key) return;
    seededFor = key;
    if (!baseBranchEdited) baseBranch = catalog?.baseBranch ?? '';
    stepMode = definition.defaultStepMode;
    const next: Record<string, unknown> = {};
    for (const input of definition.inputs ?? []) {
      const fallback = workflowSeedDefault(input);
      if (fallback !== undefined) next[input.name] = fallback;
    }
    seeds = next;
  });

  function selectProject(next: string): void {
    if (projectId === next) return;
    projectId = next;
    workflowId = '';
    seeds = {};
    seededFor = '';
    if (!baseBranchEdited) baseBranch = '';
  }

  function selectDefinition(definition: WorkflowDefinitionListing): void {
    if (!definition.valid) return;
    workflowId = definition.id;
  }

  async function submit(): Promise<void> {
    if (ungranted || !selected || validationError || submitting) return;
    submitting = true;
    const definition = selected;
    const project = projects.find((entry) => entry.project.id === projectId)?.project;
    try {
      await WorkflowStartRun(
        projectId,
        definition.id,
        definition.scope,
        goal.trim(),
        compactWorkflowSeeds(seeds),
        null,
        baseBranchEdited ? baseBranch.trim() : '',
        stepMode,
      );
      addToast('success', `Started — ${definition.name || definition.id} on ${project?.name || projectId}`);
      refreshWorkflowRunsSoon();
      onClose();
    } catch (err) {
      addToast('error', userFacingError(err, 'Could not start the run.'));
    } finally {
      submitting = false;
    }
  }
</script>

<Modal {open} title="New run" {onClose} width="lg" padding="comfortable">
  {#snippet children()}
    <form
      class="space-y-4"
      onsubmit={(event) => { event.preventDefault(); void submit(); }}
      data-testid="workflow-intake-dialog"
    >
      <fieldset>
        <legend class="mb-1 text-xs font-medium text-fg-muted">Project</legend>
        <div class="flex flex-wrap gap-1.5">
          {#each projects as entry (entry.project.id)}
            <button
              type="button"
              class={[
                'rounded-md border px-2.5 py-1.5 text-xs',
                projectId === entry.project.id ? 'border-accent bg-accent/10 text-fg' : 'border-border-subtle text-fg-muted hover:text-fg',
              ].join(' ')}
              onclick={() => selectProject(entry.project.id)}
              data-testid="workflow-intake-project"
              data-project-id={entry.project.id}
            >{getProjectLabelText(entry.project.id)}</button>
          {/each}
        </div>
      </fieldset>

      <label class="block text-xs font-medium text-fg-muted">Goal
        <textarea
          bind:value={goal}
          data-autofocus
          class="mt-1 min-h-20 w-full rounded-md border border-border-subtle bg-surface-0 p-2 text-sm text-fg"
          data-testid="workflow-intake-goal"
        ></textarea>
      </label>

      <fieldset>
        <legend class="mb-1 text-xs font-medium text-fg-muted">Workflow</legend>
        <div class="grid gap-2 sm:grid-cols-2" data-testid="workflow-intake-workflows">
          {#each definitions as definition (definition.scope + ':' + definition.id)}
            <button
              type="button"
              disabled={!definition.valid}
              class={[
                'rounded-md border p-2 text-left',
                workflowId === definition.id ? 'border-accent bg-accent/10' : 'border-border-subtle',
                definition.valid ? 'hover:bg-surface-2' : 'cursor-not-allowed opacity-45',
              ].join(' ')}
              onclick={() => selectDefinition(definition)}
              data-testid="workflow-intake-workflow"
              data-workflow-id={definition.id}
            >
              <span class="block text-sm font-medium text-fg">{definition.name || definition.id}</span>
              <span class="mt-0.5 block truncate text-xs text-fg-muted">
                {workflowDefinitionMeta(definition)}{workflowChainSummary(definition) ? ` · ${workflowChainSummary(definition)}` : ''}
              </span>
              {#if !definition.valid && definition.firstValidationError}
                <span class="mt-1 block text-xs text-fg-subtle">{definition.firstValidationError}</span>
              {/if}
            </button>
          {/each}
        </div>
        {#if definitions.length === 0}
          <p class="text-xs text-fg-muted" data-testid="workflow-intake-no-workflows">This project has no workflows yet.</p>
        {/if}
      </fieldset>

      <label class="block text-xs font-medium text-fg-muted">Base branch
        <input
          value={baseBranch}
          oninput={(event) => { baseBranch = event.currentTarget.value; baseBranchEdited = true; }}
          class="mt-1 w-full rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 font-mono text-sm text-fg"
          data-testid="workflow-intake-base-branch"
        />
      </label>

      {#if selected && (selected.inputs?.length ?? 0) > 0}
        <WorkflowSeedFields
          inputs={selected.inputs}
          {seeds}
          {browseRoot}
          disabled={ungranted}
          onChange={(name, value) => { seeds = { ...seeds, [name]: value }; }}
        />
      {/if}

      <label class="flex items-center gap-2 text-xs text-fg-muted">
        <input type="checkbox" bind:checked={stepMode} data-testid="workflow-intake-step-mode" />
        Pause at every gate
      </label>

      {#if validationError}
        <p class="text-xs text-fg-subtle" data-testid="workflow-intake-error">{validationError}</p>
      {/if}
    </form>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={onClose}>
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="primary"
      size="sm"
      testId="workflow-intake-submit"
      onclick={() => { void submit(); }}
      disabled={ungranted || Boolean(validationError) || submitting}
      loading={submitting}
    >
      {#snippet children()}{submitting ? 'Starting…' : 'Start'}{/snippet}
    </Button>
  {/snippet}
</Modal>
