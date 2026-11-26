<script lang="ts">
  // What Start work will do (docs/specs/issues.md, "Start work applies
  // per-project defaults"), each row editable and filled from the
  // project's defaults: the status to move to, assigning the ticket to
  // the person, the branch named after the ticket and the repository the
  // thread runs in, with saving them as the project's defaults. Opened
  // from Work at its button; Start work hands the plan to the run
  // (`stores/ticketWork.svelte.ts`), which Work shows.
  import { untrack } from 'svelte';
  import Button from '../primitives/Button.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { INPUT_CLASS, SELECT_CLASS } from '../settings/styles';
  import type { DetailView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { TicketBranch } from '../../stores/bindings';
  import { getProjectLabelText } from '../../stores/projects.svelte';
  import {
    defaultsAt,
    loadStartWorkDefaults,
    startWorkDefaults,
    type DefaultsPlace,
    type StartWorkPlan,
  } from '../../stores/ticketWork.svelte';
  import type { Project, Thread } from '../../types/models';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { userFacingError } from '../../utils/userFacingError';
  import { branchTemplateOf, ticketBranchName } from '../../utils/ticketBranch';

  interface Props {
    anchor: HTMLElement | undefined;
    open: boolean;
    /** The ticket's entry key (trackerTicketKey). */
    ticket: string;
    detail: DetailView;
    /** The projects a thread of the ticket can be made in (Work's): the
     * picker lists them, and only one of them is submitted. */
    projects: readonly Project[];
    /** The ticket's project: its setup holds the defaults, and the thread
     * runs in it unless another is picked. */
    project: string;
    /** Where the defaults sit in the setup; null while the connection's
     * site is not known. */
    place: DefaultsPlace | null;
    /** The connection's verified account; empty when not verified. */
    me: string;
    /** The branches Work found, to say which name is taken. */
    branches: readonly TicketBranch[];
    /** A thread Start work already made for the ticket. */
    started: Thread | null;
    /** A branch to start on and its repository, from a branch's menu;
     * null for the defaults. */
    branch: { readonly name: string; readonly projectId: string } | null;
    onStart: (plan: StartWorkPlan) => void;
    onOpenThread: (thread: Thread) => void;
    onClose: () => void;
  }

  let { anchor, open, ticket, detail, projects, project, place, me, branches, started, branch: givenBranch, onStart, onOpenThread, onClose }: Props = $props();

  const card = $derived(detail.card);
  const defaults = $derived(startWorkDefaults(ticket));
  const saved = $derived(defaults?.view && place ? defaultsAt(defaults.view.setup, place) : null);

  $effect(() => {
    if (!open) return;
    const at = { ticket, project };
    untrack(() => loadStartWorkDefaults(at.ticket, at.project));
  });

  // The rows as the person set them; null is "as the defaults say".
  let status = $state<string | null>(null);
  let assign = $state<boolean | null>(null);
  let branchName = $state<string | null>(null);
  let repository = $state<string | null>(null);
  let save = $state(false);
  $effect(() => {
    if (!open) {
      status = null;
      assign = null;
      branchName = null;
      repository = null;
      save = false;
    }
  });

  // A saved status the ticket has no move to, and is not in, is said and
  // not used.
  const savedStatus = $derived(saved?.status ?? '');
  const savedStatusMove = $derived(savedStatus ? detail.moves.find((m) => m.to.id === savedStatus) ?? null : null);
  const savedStatusUnusable = $derived(!!savedStatus && !savedStatusMove && card.status.id !== savedStatus);
  const chosenStatus = $derived(status ?? (savedStatusUnusable ? '' : savedStatus));
  const statusChoices = $derived.by(() => {
    const seen = new Set<string>();
    const out: { id: string; name: string }[] = [];
    for (const m of detail.moves) {
      if (seen.has(m.to.id)) continue;
      seen.add(m.to.id);
      out.push({ id: m.to.id, name: m.to.name || m.name });
    }
    if (chosenStatus === card.status.id && !seen.has(card.status.id)) out.unshift({ id: card.status.id, name: card.status.name || card.status.id });
    return out;
  });

  const chosenAssign = $derived(!me ? false : assign ?? (saved?.assign ?? true));
  const mine = $derived(!!me && card.assignee?.id === me);

  const defaultBranch = $derived(givenBranch?.name ?? ticketBranchName(saved?.branch ?? '', card.key, card.title));
  const chosenBranch = $derived((branchName ?? defaultBranch).trim());

  const savedRepository = $derived(saved?.repository ?? '');
  const savedRepositoryGone = $derived(!!savedRepository && !projects.some((p) => p.id === savedRepository));
  const candidate = $derived(repository ?? givenBranch?.projectId ?? (savedRepository && !savedRepositoryGone ? savedRepository : project));
  // Only a project the picker lists is submitted.
  const chosenRepository = $derived(projects.some((p) => p.id === candidate) ? candidate : '');
  const existing = $derived(branches.find((b) => b.projectId === chosenRepository && b.name === chosenBranch) ?? null);

  const projectName = $derived(getProjectLabelText(project));
  const defaultsError = $derived(defaults?.error ? userFacingError(defaults.error) : null);
  // The form is confirmed only once the project's saved defaults have
  // answered (or failed, which it says), so what it shows is what they say.
  const defaultsSettled = $derived(defaults?.project === project && !defaults.loading);

  function start(): void {
    if (!chosenBranch || !chosenRepository || !defaultsSettled) return;
    const statusName = statusChoices.find((s) => s.id === chosenStatus)?.name ?? chosenStatus;
    onStart({
      repository: chosenRepository,
      status: chosenStatus ? { id: chosenStatus, name: statusName } : null,
      assign: chosenAssign,
      me,
      branch: chosenBranch,
      save: save && place ? { project, place, branchTemplate: branchTemplateOf(chosenBranch, card.key, card.title) } : null,
    });
  }

  function close(reason?: PopoverCloseReason): void {
    onClose();
    restorePickerFocus(reason, { triggerEl: anchor });
  }

  const id = $props.id();
</script>

<Popover {anchor} {open} onClose={close} placement="bottom-start" ariaLabel="Start work on {card.key}">
  <form class="flex w-80 max-w-[90vw] flex-col gap-2.5 p-3 text-sm" data-testid="start-work-form" onsubmit={(e) => { e.preventDefault(); start(); }}>
    <p class="text-fg-muted">Start work on {card.key} does this, in order:</p>
    {#if started}
      <div class="flex items-center gap-2 rounded-[var(--radius-field)] bg-surface-1 px-2 py-1.5 text-xs text-fg-muted" data-testid="start-work-started">
        <span class="min-w-0 flex-1">{card.key} already has a thread from Start work: {started.title || 'Untitled thread'}</span>
        <Button size="xs" variant="ghost" testId="start-work-open-started" onclick={() => onOpenThread(started)}>Open thread</Button>
      </div>
    {/if}

    <label class="flex flex-col gap-1" for="{id}-repository">
      <span class="text-xs text-fg-muted">Thread in</span>
      <select id="{id}-repository" class={SELECT_CLASS} value={chosenRepository} onchange={(e) => { repository = (e.target as HTMLSelectElement).value; }} data-testid="start-work-repository">
        {#each projects as p (p.id)}
          <option value={p.id}>{getProjectLabelText(p.id)}</option>
        {/each}
      </select>
      {#if savedRepositoryGone}
        <span class="text-xs text-warning" data-testid="start-work-repository-gone">The saved repository is no longer a project on this computer.</span>
      {/if}
    </label>

    <label class="flex flex-col gap-1" for="{id}-branch">
      <span class="text-xs text-fg-muted">On the branch, in a worktree</span>
      <input id="{id}-branch" class={INPUT_CLASS} value={branchName ?? defaultBranch} oninput={(e) => { branchName = (e.target as HTMLInputElement).value; }} spellcheck="false" autocomplete="off" data-testid="start-work-branch" />
      {#if existing}
        <span class="text-xs text-fg-muted" data-testid="start-work-branch-exists">
          {existing.worktreePath ? `This branch exists, checked out at ${existing.worktreePath}: the thread runs there.` : 'This branch exists: the thread runs on it in a new worktree.'}
        </span>
      {/if}
    </label>

    <label class="flex flex-col gap-1" for="{id}-status">
      <span class="text-xs text-fg-muted">Move the ticket to</span>
      <select id="{id}-status" class={SELECT_CLASS} value={chosenStatus} onchange={(e) => { status = (e.target as HTMLSelectElement).value; }} data-testid="start-work-status">
        <option value="">Do not move</option>
        {#each statusChoices as s (s.id)}
          <option value={s.id}>{s.name}</option>
        {/each}
      </select>
      {#if chosenStatus && chosenStatus === card.status.id}
        <span class="text-xs text-fg-muted" data-testid="start-work-status-already">Already in {card.status.name}.</span>
      {:else if savedStatusUnusable && status === null}
        <span class="text-xs text-warning" data-testid="start-work-status-unusable">The saved status {savedStatus} is not a move of this ticket now.</span>
      {/if}
    </label>

    <label class="flex items-center gap-2 compact:min-h-9">
      <input type="checkbox" checked={chosenAssign} disabled={!me} onchange={(e) => { assign = (e.target as HTMLInputElement).checked; }} data-testid="start-work-assign" />
      <span>Assign it to me</span>
      {#if mine && chosenAssign}<span class="text-xs text-fg-muted" data-testid="start-work-assign-already">Already yours.</span>{/if}
    </label>
    {#if !me}
      <span class="text-xs text-fg-muted" data-testid="start-work-assign-unverified">Its connection has no verified account, so it cannot be assigned to you from here.</span>
    {/if}

    <label class="flex items-center gap-2 compact:min-h-9">
      <input type="checkbox" bind:checked={save} disabled={!place || !defaults?.view} data-testid="start-work-save" />
      <span>Save as defaults for {projectName}</span>
    </label>
    {#if defaultsError}
      <span class="text-xs text-error" role="alert" data-testid="start-work-defaults-error">Could not read the saved defaults: {defaultsError}</span>
    {:else if defaults?.loading && !defaults.view}
      <span class="text-xs text-fg-muted">Reading the saved defaults…</span>
    {/if}

    <div class="flex justify-end gap-2 pt-1">
      <Button size="sm" variant="ghost" testId="start-work-cancel" onclick={() => close()}>Cancel</Button>
      <Button size="sm" variant="primary" type="submit" disabled={!chosenBranch || !chosenRepository || !defaultsSettled} title={!chosenRepository ? 'Pick the repository the thread runs in' : defaultsSettled ? undefined : 'Waiting for the saved defaults'} testId="start-work-go">Start work</Button>
    </div>
  </form>
</Popover>
