<script lang="ts">
  import { untrack } from 'svelte';
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import AddProjectModal from '../sidebar/AddProjectModal.svelte';
  import ConversationTransferStatus from './ConversationTransferStatus.svelte';
  import {
    closeConversationTransfer, computerTransfers, submitConversationTransfer,
    supportsConversationTransfer, type TransferRequest, type TransferKind,
  } from '../../stores/conversationTransfers.svelte';
  import { backendDisplayName, backendReachable, getAttachedBackends } from '../../stores/attachedBackends.svelte';
  import { getProject, getProjects, projectSiblingOn } from '../../stores/projects.svelte';
  import { projectBackend } from '../../transport/entityIndex';
  import { hasScope } from '../../transport/scopes';
  import type { BackendKey } from '../../transport/backendKey';
  import type { Project } from '../../types/models';

  let { request }: { request: TransferRequest } = $props();
  let computers = $derived(getAttachedBackends().filter((entry) => entry.id !== request.source));
  let destination = $state<BackendKey>(untrack(() => request.destination ?? computers.find((entry) => backendReachable(entry.id) && supportsConversationTransfer(entry.id) && hasScope('threads:operate', entry.id))?.id ?? ''));
  let kind = $state<TransferKind>(untrack(() => request.kind ?? 'move'));
  let includeWorkspace = $state(untrack(() => request.includeWorkspace ?? true));
  let projectID = $state(untrack(() => request.projectID ?? sibling(destination)));
  let adding = $state(false);
  let projects = $derived(getProjects().filter((entry) => projectBackend(entry.project.id) === destination));
  let row = $derived(computerTransfers(request.source).rows.find((row) => row.id === request.operationID));
  let running = $derived(Boolean(row && (!row.needsDestination || row.cancelRequested || row.phase === 'canceled')));
  let locked = $derived(request.submitted || Boolean(request.intent));
  let ready = $derived(Boolean(destination && projectID) && backendReachable(request.source) && backendReachable(destination) &&
    supportsConversationTransfer(destination) && hasScope('threads:operate', request.source) && hasScope('threads:operate', destination) &&
    (!includeWorkspace || hasScope('git:operate', destination)));
  const fieldClass = 'min-w-0 w-full rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-2 text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';

  function sibling(backend: BackendKey): string {
    return request.thread.projectId ? projectSiblingOn(request.thread.projectId, backend)?.project.id ?? '' : '';
  }
  function selectDestination(value: BackendKey): void {
    destination = value;
    projectID = sibling(value);
  }
  function useProject(project: Project): void {
    if (projectBackend(project.id) === destination) projectID = project.id;
    adding = false;
  }
</script>

<Modal open={!adding} title="Move or copy conversation" onClose={closeConversationTransfer} width="md">
  <div class="flex flex-col gap-4 text-sm min-w-0">
    <p class="truncate text-fg-muted" title={request.thread.title}>{request.thread.title || 'Untitled conversation'}</p>
    {#if running && row}
      <ConversationTransferStatus backend={request.source} {row} showTitle={false} />
    {:else}
      <fieldset class="grid grid-cols-2 gap-2" disabled={locked}>
        <legend class="sr-only">Transfer type</legend>
        {#each ['move', 'copy'] as option}
          <label class={['flex cursor-pointer items-center gap-2 rounded-[var(--radius-field)] border px-3 py-2', kind === option ? 'border-accent bg-accent/10' : 'border-border-subtle', locked ? 'cursor-default' : ''].join(' ')}>
            <input type="radio" name="transfer-kind" value={option} bind:group={kind} class="accent-accent" />
            {option === 'move' ? 'Move' : 'Copy / fork'}
          </label>
        {/each}
      </fieldset>
      <p class="text-xs text-fg-muted">{kind === 'move' ? 'Continue the same conversation on another computer. It stays in your sidebar and follows its new home.' : 'Create an independent conversation on another computer. Keep using the original here.'}</p>
      <label class="flex flex-col gap-1.5 text-xs text-fg-muted">
        Computer
        <select class={fieldClass} value={destination} disabled={locked} onchange={(event) => selectDestination(event.currentTarget.value)}>
          <option value="" disabled>Choose a computer</option>
          {#each computers as computer (computer.id)}
            {@const offline = !backendReachable(computer.id)}
            {@const supported = supportsConversationTransfer(computer.id)}
            {@const granted = hasScope('threads:operate', computer.id)}
            <option value={computer.id} disabled={offline || !supported || !granted}>
              {backendDisplayName(computer)}{offline ? ' · Offline' : !supported ? ' · Update required' : !granted ? ' · No access' : ''}
            </option>
          {/each}
        </select>
      </label>
      <label class="flex flex-col gap-1.5 text-xs text-fg-muted">
        Project on destination
        <select class={fieldClass} bind:value={projectID} disabled={request.submitted || !destination}>
          <option value="" disabled>Choose a project</option>
          {#each projects as entry (entry.project.id)}
            <option value={entry.project.id}>{entry.project.name} · {entry.project.path}</option>
          {/each}
        </select>
      </label>
      {#if !request.submitted && destination && hasScope('git:operate', destination)}
        <div><Button variant="ghost" size="xs" disabled={!backendReachable(destination)} onclick={() => { adding = true; }}>Add an existing project…</Button></div>
      {/if}
      <label class="flex items-start gap-2 text-xs text-fg">
        <input type="checkbox" bind:checked={includeWorkspace} disabled={locked} class="mt-0.5 accent-accent" />
        <span>Include workspace changes<span class="mt-1 block text-fg-muted">Creates a separate worktree with commits, staged changes and untracked files. Ignored files stay on this computer.</span></span>
      </label>
      {#if !includeWorkspace}<p class="text-xs text-fg-muted">The conversation will use the destination project’s existing checkout.</p>{/if}
      {#if includeWorkspace && destination && !hasScope('git:operate', destination)}<p class="text-xs text-warning">Workspace access is needed on the destination to include these changes.</p>{/if}
      {#if !backendReachable(request.source) || (destination && !backendReachable(destination))}<p role="status" class="text-xs text-warning">Reconnect both computers to finish setup.</p>{/if}
    {/if}
    {#if request.error}<p role="alert" class="break-words text-xs text-error">{request.error}</p>{/if}
  </div>
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={closeConversationTransfer}>Close</Button>
    {#if !running}
      <Button variant="primary" size="sm" disabled={!ready} loading={request.submitting} onclick={() => void submitConversationTransfer(destination, projectID, kind, includeWorkspace)}>
        {request.submitting ? 'Starting…' : request.submitted || request.intent ? 'Finish setup' : kind === 'move' ? 'Move conversation' : 'Copy conversation'}
      </Button>
    {/if}
  {/snippet}
</Modal>

{#if adding}
  <AddProjectModal open={true} initialBackend={destination} lockBackend onClose={() => { adding = false; }} onCreated={useProject} onDuplicate={(id) => { const project = getProject(id)?.project; if (project) useProject(project); }} />
{/if}
