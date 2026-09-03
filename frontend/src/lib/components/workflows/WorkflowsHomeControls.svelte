<script lang="ts">
  // Home header controls (UI-SPEC §3.1). Pause-all is the one global kill
  // switch — not a queue: no ordering, no counts. The project filter is view
  // state, and intake is the one entry point: D32 removed the studio-thread and
  // triage-agent spawners.
  //
  // Remote posture (§10): every control here mutates, so all of them disable
  // with a "Not granted to this device" tooltip in a view-only session; the project filter is
  // pure view state and stays live.

  import { WorkflowSetGlobalPause } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { getProjectLabelText, getProjects } from '../../stores/projects.svelte';
  import { hasScope } from '../../transport/scopes';
  import { isWorkflowEnginePaused } from '../../stores/workflowRuns.svelte';
  import {
    getWorkflowProjectFilter,
    setWorkflowProjectFilter,
    setWorkflowsOverlayDialog,
  } from '../../stores/workflowsOverlay.svelte';

  // Every control here drives the workflow engine, which is `threads:autonomy`.
  let ungranted = $derived(!hasScope('threads:autonomy'));
  let paused = $derived(isWorkflowEnginePaused());
  let projects = $derived(getProjects());
  let filter = $derived(getWorkflowProjectFilter());
  let pausing = $state(false);

  const ungrantedTitle = $derived(ungranted ? 'Not granted to this device' : undefined);

  async function togglePause(): Promise<void> {
    if (ungranted || pausing) return;
    pausing = true;
    const next = !paused;
    try {
      await WorkflowSetGlobalPause(next);
      addToast('info', next ? 'Paused — no new phases start; in-flight turns finish' : 'Resumed — phases start again');
    } catch (err) {
      addToast('error', userFacingError(err, 'Could not change the global pause.'));
    } finally {
      pausing = false;
    }
  }
</script>

<div class="flex flex-wrap items-center gap-2 border-b border-border-subtle px-4 py-2.5" data-testid="workflows-controls">
  <button
    class="rounded-md border border-border-subtle px-2 py-1 text-xs text-fg-muted hover:text-fg disabled:cursor-not-allowed disabled:opacity-50"
    onclick={togglePause}
    disabled={ungranted || pausing}
    title={ungrantedTitle ?? 'Pause stops new phase starts everywhere; in-flight turns finish'}
    data-testid="workflows-pause-all"
    aria-pressed={paused}
  >{paused ? '▶ Resume all' : '❚❚ Pause all'}</button>

  <label class="flex items-center gap-1.5 text-xs text-fg-muted">
    <span class="sr-only">Project</span>
    <select
      class="rounded-md border border-border-subtle bg-surface-0 px-2 py-1 text-xs"
      value={filter}
      onchange={(event) => setWorkflowProjectFilter((event.currentTarget as HTMLSelectElement).value)}
      data-testid="workflows-project-filter"
    >
      <option value="">All projects</option>
      {#each projects as entry (entry.project.id)}
        <option value={entry.project.id}>{getProjectLabelText(entry.project.id)}</option>
      {/each}
    </select>
  </label>

  <div class="ml-auto flex items-center gap-2">
    <button
      class="rounded-md border border-border-subtle px-2 py-1 text-xs text-fg-muted hover:text-fg disabled:cursor-not-allowed disabled:opacity-50"
      onclick={() => setWorkflowsOverlayDialog('intake')}
      disabled={ungranted}
      title={ungrantedTitle}
      data-testid="workflows-new-run"
    >+ New run</button>
  </div>
</div>
