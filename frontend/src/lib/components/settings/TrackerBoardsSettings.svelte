<script lang="ts">
  // Settings > Issues > Boards: the boards a project's Issues pane lists,
  // in order, and which one opens first. A project lives on one computer
  // and lists only that computer's connections' boards, so the projects
  // offered are the page's computer's. Every change saves the whole list
  // at once and the list shown is the one the computer stored.
  import { onMount, untrack } from 'svelte';
  import ArrowDown from '@lucide/svelte/icons/arrow-down';
  import ArrowUp from '@lucide/svelte/icons/arrow-up';
  import { settingsComputer } from './settingsComputer';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import { HOME_BACKEND } from '../../transport/backendKey';
  import { projectBackend } from '../../transport/entityIndex';
  import { getProjects, isLoaded } from '../../stores/projects.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { acquireTrackerConnections, refreshTrackerConnections, trackerConnectionsOf, trackerReadGate } from '../../stores/trackerConnections.svelte';
  import {
    acquireTrackerProjectBoards,
    refreshTrackerProjectBoards,
    saveTrackerProjectBoards,
    trackerProjectBoardsOf,
    withBoardAdded,
    withBoardMoved,
    withBoardRemoved,
    withOpensFirst,
  } from '../../stores/trackerProjectBoards.svelte';
  import {
    requestedTrackerBoardsProject,
    takeRequestedTrackerBoardsProject,
  } from '../../stores/settingsOverlay.svelte';
  import type { TrackerBoardChoice, TrackerConnection, TrackerProjectBoard } from '../../stores/bindings';
  import { disambiguatedPathLabels, formatPathLabel } from '../../utils/pathDisplay';
  import { userFacingError } from '../../utils/userFacingError';
  import IconButton from '../primitives/IconButton.svelte';
  import Icon from '../primitives/Icon.svelte';
  import SettingsField from './SettingsField.svelte';
  import TrackerBoardAdd from './TrackerBoardAdd.svelte';
  import SettingsOlderComputer from './SettingsOlderComputer.svelte';
  import { GHOST_BUTTON_CLASS, SELECT_CLASS } from './styles';
  import { trackerConnectionName, trackerKindName } from './trackerConnectionStatus';

  const { backend, scopeGrant } = settingsComputer();

  onMount(() => acquireTrackerConnections(backend));

  let projects = $derived(getProjects().filter((row) => (projectBackend(row.project.id) ?? HOME_BACKEND) === backend));
  let labels = $derived(disambiguatedPathLabels(projects.map((row) => row.project)));
  let projectsLoaded = $derived(isLoaded());
  let selectedId = $state('');

  // A deep link's project wins once it is listed; otherwise keep the pick
  // while it exists, or fall back to the first project.
  $effect(() => {
    const requested = requestedTrackerBoardsProject();
    if (requested !== null && projects.some((p) => p.project.id === requested)) {
      selectedId = requested;
      takeRequestedTrackerBoardsProject();
      return;
    }
    if (projects.length === 0) {
      selectedId = '';
      return;
    }
    if (!projects.some((p) => p.project.id === untrack(() => selectedId))) selectedId = projects[0].project.id;
  });

  $effect(() => {
    const id = selectedId;
    if (!id) return;
    return untrack(() => acquireTrackerProjectBoards(backend, id));
  });

  let connectionsView = $derived(trackerConnectionsOf(backend));
  let connections = $derived(connectionsView.list);
  // A listed board's connection, as the held connection list has it: the
  // one source of its name and of whether it is on, which follows every
  // change of the connections. Until that list loads, the board answer's
  // copy stands in.
  let connectionById = $derived(new Map<string, TrackerConnection>(connections.map((c) => [c.id, c])));
  let boards = $derived(selectedId ? trackerProjectBoardsOf(backend, selectedId) : null);
  let list = $derived(boards?.list ?? []);
  let choosable = $derived(connections.filter((c) => c.enabled && c.kind !== 'tempo'));
  let listed = $derived(new Set(list.map((b) => `${b.connection}\u0000${b.board}`)));

  let gate = $derived(trackerReadGate(connectionsView));
  let operateGrant = $derived(scopeGrant('tracker:operate'));
  let operateReason = $derived(
    operateGrant === 'granted'
      ? ''
      : operateGrant === 'refused'
        ? "This device may not change a project's boards on this computer."
        : (grantBlockedTitle(operateGrant) ?? ''),
  );

  let saveError = $state<unknown>(null);
  // A save error belongs to the project it was made for.
  $effect(() => {
    void selectedId;
    saveError = null;
  });

  // An edit is built only from the stored list: not while a save runs, nor
  // after a read or a save failed until a read answers.
  let locked = $derived(operateReason !== '' || (boards?.saving ?? false) || !(boards?.current ?? false));

  async function save(next: TrackerProjectBoard[]): Promise<boolean> {
    const id = selectedId;
    saveError = null;
    try {
      await saveTrackerProjectBoards(backend, id, next);
      return true;
    } catch (err) {
      if (id === selectedId) saveError = err;
      return false;
    }
  }

  function connectionNameOf(board: TrackerBoardChoice): string {
    const connection = connectionById.get(board.connection);
    if (connection) return trackerConnectionName(connection);
    return board.connection_label || trackerKindName(board.connection_kind);
  }

  function connectionOn(board: TrackerBoardChoice): boolean {
    return connectionById.get(board.connection)?.enabled ?? board.connection_enabled;
  }

  // Why Add cannot open, '' when it can. The page shows its boards only
  // once the computer answered its connections (trackerReadGate).
  let addUnavailable = $derived(choosable.length === 0 ? 'Add a Jira or Linear connection first, and turn it on.' : '');
</script>

<div class="settings-sections flex flex-col gap-4" data-testid="settings-tracker-boards">
  {#if gate === 'older'}
    <SettingsOlderComputer {backend} lacks="issue trackers" />
  {:else if gate === 'refused'}
    <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-boards-refused">
      This device may not read a project's boards on this computer.
    </p>
  {:else if gate === 'waiting'}
    <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-boards-waiting">Asking this computer about its trackers…</p>
  {:else if gate === 'failed'}
    {@render connectionsFailure()}
  {:else if projectsLoaded && projects.length === 0}
    <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-boards-no-projects">
      No projects yet. Add one from the sidebar first.
    </p>
  {:else}
    <SettingsField
      id="tracker-boards.project"
      label="Project"
      hint="Choose the project whose boards to list."
      htmlFor="settings-tracker-boards-project"
      stacked={isCompactLayout()}
    >
      <select
        id="settings-tracker-boards-project"
        data-testid="settings-tracker-boards-project"
        class={SELECT_CLASS}
        value={selectedId}
        onchange={(e) => (selectedId = (e.target as HTMLSelectElement).value)}
      >
        {#each projects as entry (entry.project.id)}
          <option value={entry.project.id}>{formatPathLabel(labels.get(entry.project.id)!)}</option>
        {/each}
      </select>
    </SettingsField>

    {#if boards}
      <div
        class="flex flex-col gap-3"
        data-settings-field="tracker-boards.boards"
        data-settings-label="Boards"
      >
        <p class="text-[0.8125rem] font-medium text-fg">Boards</p>
        {#if boards.error !== null}
          <div class="flex flex-wrap items-center gap-2" role="alert">
            <p class="text-[0.75rem] text-error" data-testid="settings-tracker-boards-error">The boards could not be read: {userFacingError(boards.error)}</p>
            <button
              type="button"
              class={GHOST_BUTTON_CLASS}
              onclick={() => void refreshTrackerProjectBoards(backend, selectedId)}
              data-testid="settings-tracker-boards-retry"
            >Try again</button>
          </div>
        {/if}
        {#if !boards.loaded}
          {#if boards.error === null}
            <p class="text-[0.75rem] text-fg-muted">Loading boards…</p>
          {/if}
        {:else if list.length === 0}
          <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-boards-empty">
            This project lists no boards. Add one to see it in the project's Issues pane.
          </p>
        {:else}
          <ol class="flex flex-col gap-2" data-testid="settings-tracker-boards-list">
            {#each list as board, index (`${board.connection}\u0000${board.board}`)}
              <li class="flex flex-col gap-1.5" data-testid="tracker-board-row" data-board={board.board}>
                <div class="flex min-w-0 flex-wrap items-baseline gap-x-2">
                  <span class="truncate text-[0.8125rem] font-medium text-fg" data-testid="tracker-board-name">{board.name || `Board ${board.board}`}</span>
                  {#if board.opens_first}
                    <span class="text-[0.6875rem] text-accent" data-testid="tracker-board-opens-first">Opens first</span>
                  {/if}
                  {#if !connectionOn(board)}
                    <span class="text-[0.6875rem] text-warning" data-testid="tracker-board-connection-off">Connection off</span>
                  {/if}
                </div>
                <span class="truncate text-[0.71875rem] text-fg-hint">
                  {#if board.tracker_project}{`${board.tracker_project} · `}{/if}{connectionNameOf(board)}
                </span>
                <div class="flex flex-wrap items-center gap-1.5">
                  <IconButton
                    label="Move {board.name || 'board'} up"
                    size="sm"
                    disabled={locked || index === 0}
                    testId="tracker-board-up"
                    onClick={() => void save(withBoardMoved(list, index, -1))}
                  >
                    {#snippet children()}<Icon icon={ArrowUp} size={14} />{/snippet}
                  </IconButton>
                  <IconButton
                    label="Move {board.name || 'board'} down"
                    size="sm"
                    disabled={locked || index === list.length - 1}
                    testId="tracker-board-down"
                    onClick={() => void save(withBoardMoved(list, index, 1))}
                  >
                    {#snippet children()}<Icon icon={ArrowDown} size={14} />{/snippet}
                  </IconButton>
                  {#if !board.opens_first}
                    <button
                      type="button"
                      class={GHOST_BUTTON_CLASS}
                      disabled={locked}
                      title={operateReason || undefined}
                      onclick={() => void save(withOpensFirst(list, index))}
                      data-testid="tracker-board-open-first"
                    >Open first</button>
                  {/if}
                  <button
                    type="button"
                    class={GHOST_BUTTON_CLASS}
                    disabled={locked}
                    title={operateReason || undefined}
                    onclick={() => void save(withBoardRemoved(list, index))}
                    data-testid="tracker-board-remove"
                  >Remove</button>
                </div>
              </li>
            {/each}
          </ol>
        {/if}
        {#if saveError !== null}
          <p class="text-[0.75rem] text-error" role="alert" data-testid="settings-tracker-boards-save-error">{userFacingError(saveError)}</p>
          <!-- A failed save reads the list again; a read that fails says so above. -->
          {#if boards.error === null}
            <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-boards-reread">
              {boards.current ? 'The list shown is the saved one; make the change again.' : 'Reading the saved list…'}
            </p>
          {/if}
        {/if}
        {#if connectionsView.error !== null}
          {@render connectionsFailure()}
        {/if}
        {#if boards.loaded}
          {#key selectedId}
            <TrackerBoardAdd
              {backend}
              connections={choosable}
              {listed}
              disabled={locked}
              disabledReason={operateReason || undefined}
              unavailable={addUnavailable}
              onAdd={(board) => save(withBoardAdded(list, board))}
            />
          {/key}
        {/if}
      </div>
    {/if}
  {/if}
</div>

{#snippet connectionsFailure()}
  <div class="flex flex-wrap items-center gap-2" role="alert">
    <p class="text-[0.75rem] text-error" data-testid="settings-tracker-boards-connections-error">{userFacingError(connectionsView.error)}</p>
    <button
      type="button"
      class={GHOST_BUTTON_CLASS}
      onclick={() => void refreshTrackerConnections(backend)}
      data-testid="settings-tracker-boards-connections-retry"
    >Try again</button>
  </div>
{/snippet}
