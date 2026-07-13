<script lang="ts">
  import { untrack } from 'svelte';
  import type { WorkflowDefinitionView } from '../../types/workflow';
  import Modal from '../primitives/Modal.svelte';
  import DirectoryBrowser from '../sidebar/DirectoryBrowser.svelte';
  import { WorkflowEnqueueItem, WorkflowListDefinitions } from '../../stores/bindings';
  import { getProjects } from '../../stores/projects.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { compactWorkflowSeeds, workflowIntakeError } from '../../stores/workflowIntake';
  import { workflowDefinitionMeta } from '../../stores/workflowData';
  import { userFacingError } from '../../utils/userFacingError';
  import { isViewOnlySession } from '../../transport/runMode';
  import {
    getWorkflowIntakePrefill,
    loadWorkflowOverview,
  } from '../../stores/workflowsPane.svelte';
  import { refreshWorkflowsSidebar } from '../../stores/workflowsSidebar.svelte';

  interface Props { open: boolean; onClose: () => void }
  let { open, onClose }: Props = $props();
  let projects = $derived(getProjects());
  let definitions: WorkflowDefinitionView[] = $state([]);
  let definitionsError: string | null = $state(null);
  let definitionRequestVersion = 0;
  let projectId = $state('');
  let goal = $state('');
  let workflowId = $state('');
  let seeds: Record<string, unknown> = $state({});
  let baseBranch = $state('');
  let baseBranchEdited = $state(false);
  let baseBranchSource = '';
  let prefillBaseBranch = '';
  let prefillProjectId = '';
  let stepMode = $state(false);
  let submitting = $state(false);
  let viewOnly = $derived(isViewOnlySession());
  let pathPickerFor: string | null = $state(null);
  let wasOpen = false;

  let projectDefinitions = $derived(definitions.filter((entry) => entry.projectId === projectId));
  let selectedView = $derived(projectDefinitions.find((entry) => entry.definition.id === workflowId) ?? null);
  let selected = $derived(selectedView?.definition ?? null);
  let validationError = $derived(workflowIntakeError({ projectId, goal, definition: selected, seeds }));
  let predictedPosition = $derived(selectedView?.catalog.predictedQueuePosition ?? 1);

  function parseDefault(value: unknown): unknown {
    if (typeof value !== 'string') return value;
    try { return JSON.parse(value); } catch { return value; }
  }

  async function loadDefinitions(project: string): Promise<void> {
    const version = ++definitionRequestVersion;
    definitions = [];
    definitionsError = null;
    if (!project) return;
    try {
      const catalog = await WorkflowListDefinitions(project);
      if (version !== definitionRequestVersion || project !== projectId) return;
      definitions = catalog.workflows.map((definition) => ({ projectId: project, catalog, definition }));
    } catch (error) {
      if (version !== definitionRequestVersion || project !== projectId) return;
      definitionsError = userFacingError(error, 'Could not load workflows for this project.');
      addToast('error', definitionsError);
    }
  }

  function initialize(): void {
    const prefill = getWorkflowIntakePrefill();
    projectId = prefill?.projectId ?? projects[0]?.project.id ?? '';
    goal = prefill?.goal ?? '';
    workflowId = prefill?.workflowId ?? '';
    seeds = { ...(prefill?.seeds ?? {}) };
    prefillBaseBranch = prefill?.baseBranch ?? '';
    prefillProjectId = prefill?.projectId ?? projectId;
    baseBranch = prefillBaseBranch;
    baseBranchEdited = Boolean(prefillBaseBranch);
    baseBranchSource = '';
    stepMode = prefill?.stepMode ?? false;
    pathPickerFor = null;
    void loadDefinitions(projectId);
  }

  function selectProject(nextProjectId: string): void {
    if (projectId === nextProjectId) return;
    projectId = nextProjectId;
    workflowId = '';
    seeds = {};
    void loadDefinitions(nextProjectId);
  }

  $effect(() => {
    if (open && !wasOpen) initialize();
    wasOpen = open;
  });

  $effect(() => {
    const view = selectedView;
    if (!view) return;
    const source = `${view.projectId}\n${view.definition.id}`;
    if (baseBranchSource !== source) {
      const prefill = view.projectId === prefillProjectId ? prefillBaseBranch : '';
      baseBranch = prefill || view.catalog.baseBranch;
      baseBranchEdited = Boolean(prefill);
      baseBranchSource = source;
    }
    stepMode = getWorkflowIntakePrefill()?.stepMode ?? view.definition.defaultStepMode;
    const next = { ...untrack(() => seeds) };
    let changed = false;
    for (const input of view.definition.inputs) {
      if (next[input.name] === undefined && input.default !== undefined) {
        next[input.name] = parseDefault(input.default);
        changed = true;
      }
      if (next[input.name] === undefined && input.type === 'boolean') {
        next[input.name] = false;
        changed = true;
      }
    }
    if (changed) seeds = next;
  });

  function selectDefinition(view: WorkflowDefinitionView): void {
    if (!view.definition.valid) return;
    workflowId = view.definition.id;
    const allowed = new Set(view.definition.inputs.map((input) => input.name));
    seeds = Object.fromEntries(Object.entries(seeds).filter(([name]) => allowed.has(name)));
  }

  function updateSeed(name: string, value: unknown): void { seeds = { ...seeds, [name]: value }; }

  function selectSeedFile(name: string, path: string): void {
    updateSeed(name, path);
    pathPickerFor = null;
  }

  async function submit(): Promise<void> {
    if (viewOnly || !selected || validationError || submitting) return;
    submitting = true;
    try {
      await WorkflowEnqueueItem(
        projectId, selected.id, selected.scope, goal.trim(),
        compactWorkflowSeeds(seeds), null, baseBranchEdited ? baseBranch.trim() : '', stepMode,
      );
      addToast('success', `Queued — position ${predictedPosition} · starts when a slot frees`);
      onClose();
      refreshWorkflowsSidebar();
      await loadWorkflowOverview();
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not queue the run.'));
    } finally {
      submitting = false;
    }
  }
</script>

<Modal {open} title="New run" {onClose} width="lg">
  <form class="space-y-4" onsubmit={(event) => { event.preventDefault(); void submit(); }} data-testid="wf-intake-dialog">
    <fieldset>
      <legend class="mb-1 text-xs font-medium text-fg-muted">Project</legend>
      <div class="flex flex-wrap gap-1.5" data-testid="wf-intake-projects">
        {#each projects as project}
          <button type="button" class={projectId === project.project.id ? 'rounded-md border border-accent bg-accent/10 px-2.5 py-1.5 text-xs' : 'rounded-md border border-border-subtle px-2.5 py-1.5 text-xs'} onclick={() => selectProject(project.project.id)} data-testid="wf-intake-project">● {project.project.name}</button>
        {/each}
      </div>
    </fieldset>

    <label class="block text-xs font-medium text-fg-muted">Goal
      <textarea bind:value={goal} class="mt-1 min-h-24 w-full rounded-md border border-border-subtle bg-surface-0 p-2 text-sm text-fg" data-testid="wf-intake-goal"></textarea>
    </label>

    <fieldset>
      <legend class="mb-1 text-xs font-medium text-fg-muted">Workflow</legend>
      <div class="grid gap-2 sm:grid-cols-2" data-testid="wf-intake-workflows">
        {#each projectDefinitions as view (`${view.projectId}:${view.definition.id}`)}
          <button type="button" disabled={!view.definition.valid} class={["rounded-md border p-2 text-left", workflowId === view.definition.id ? 'border-accent bg-accent/10' : 'border-border-subtle', view.definition.valid ? 'hover:bg-surface-2' : 'cursor-not-allowed opacity-45'].join(' ')} onclick={() => selectDefinition(view)} title={view.definition.valid ? '' : view.definition.firstValidationError} data-testid="wf-intake-workflow">
            <span class="block text-sm font-medium">{view.definition.name}</span>
            <span class="mt-0.5 block text-xs text-fg-muted">{workflowDefinitionMeta(view.definition)} · {view.definition.phases.map((phase) => phase.id).join(' → ')}</span>
            {#if !view.definition.valid}<span class="mt-1 block text-xs text-error">{view.definition.firstValidationError}</span>{/if}
          </button>
        {/each}
      </div>
      {#if definitionsError}<p class="mt-1 text-xs text-error" data-testid="wf-intake-definitions-error">{definitionsError}</p>{/if}
    </fieldset>

    <label class="block text-xs font-medium text-fg-muted">Base branch
      <input value={baseBranch} oninput={(event) => { baseBranch = (event.currentTarget as HTMLInputElement).value; baseBranchEdited = true; }} class="mt-1 w-full rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 font-mono text-sm text-fg" data-testid="wf-intake-base-branch" />
    </label>

    {#if selected}
      <div class="grid gap-3 sm:grid-cols-2" data-testid="wf-intake-seeds">
        {#each selected.inputs as input (input.name)}
          {#if input.type === 'boolean'}
            <label class="flex items-center gap-2 text-xs"><input type="checkbox" checked={seeds[input.name] === true} onchange={(event) => updateSeed(input.name, (event.currentTarget as HTMLInputElement).checked)} data-testid={`wf-seed-${input.name}`} /> {input.name}{#if !input.required}<span class="text-fg-hint">(optional)</span>{/if}</label>
          {:else if input.enum}
            <label class="text-xs text-fg-muted">{input.name}{#if !input.required} <span class="text-fg-hint">(optional)</span>{/if}
              <select value={String(seeds[input.name] ?? '')} onchange={(event) => updateSeed(input.name, (event.currentTarget as HTMLSelectElement).value)} class="mt-1 w-full rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-sm" data-testid={`wf-seed-${input.name}`}>
                <option value="">Choose…</option>{#each input.enum as option}<option value={String(option)}>{String(option)}</option>{/each}
              </select>
            </label>
          {:else if input.multiline}
            <label class="text-xs text-fg-muted sm:col-span-2">{input.name}{#if !input.required} <span class="text-fg-hint">(optional)</span>{/if}<textarea value={String(seeds[input.name] ?? '')} oninput={(event) => updateSeed(input.name, (event.currentTarget as HTMLTextAreaElement).value)} class="mt-1 min-h-20 w-full rounded-md border border-border-subtle bg-surface-0 p-2 text-sm" data-testid={`wf-seed-${input.name}`}></textarea></label>
          {:else}
            <label class="text-xs text-fg-muted">{input.name}{#if !input.required} <span class="text-fg-hint">(optional)</span>{/if}
              <div class="mt-1 flex gap-1">
                <input type={input.type === 'number' ? 'number' : 'text'} value={String(seeds[input.name] ?? '')} oninput={(event) => { const raw = (event.currentTarget as HTMLInputElement).value; updateSeed(input.name, input.type === 'number' && raw !== '' ? Number(raw) : raw); }} class="min-w-0 flex-1 rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-sm" data-testid={`wf-seed-${input.name}`} />
                {#if input.format === 'path'}<button type="button" class="rounded-md border border-border-subtle px-2 text-xs disabled:opacity-45" disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} onclick={() => { pathPickerFor = pathPickerFor === input.name ? null : input.name; }} data-testid={`wf-seed-${input.name}-pick`}>Browse</button>{/if}
              </div>
            </label>
          {/if}
          {#if pathPickerFor === input.name}
            <div class="sm:col-span-2 rounded-md border border-border-subtle p-2"><DirectoryBrowser initialPath={String(seeds[input.name] ?? projects.find((entry) => entry.project.id === projectId)?.project.path ?? '~')} onSelect={(path) => updateSeed(input.name, path)} onSelectFile={(path) => selectSeedFile(input.name, path)} /></div>
          {/if}
        {/each}
      </div>
    {/if}

    <label class="flex items-center gap-2 text-xs"><input type="checkbox" bind:checked={stepMode} data-testid="wf-intake-step-mode" /> Pause at every gate</label>

    {#if validationError}<p class="text-xs text-error" data-testid="wf-intake-error">{validationError}</p>{/if}
    <div class="flex justify-end gap-2 border-t border-border-subtle pt-3">
      <button type="button" class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={onClose} data-testid="wf-intake-cancel">Cancel</button>
      <button class="rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-white disabled:opacity-45" disabled={viewOnly || Boolean(validationError) || submitting} title={viewOnly ? 'Local only' : undefined} data-testid="wf-intake-submit">Queue — position {predictedPosition}</button>
    </div>
  </form>
</Modal>
