<script lang="ts">
  // Adding a board to a project's list: a connection, then one of the
  // tracker projects (Linear teams) its user can see, then one of that
  // project's boards. Both lists are asked of the tracker while the
  // picker is open and dropped when it closes. A failed or too-slow list
  // shows the computer's sentence with Try again.
  import {
    TrackerBoards,
    TrackerProjects,
    type TrackerBoardInfo,
    type TrackerConnection,
    type TrackerProjectBoard,
    type TrackerProjectView,
  } from '../../stores/bindings';
  import type { BackendKey } from '../../transport/backendKey';
  import { withBackendTarget } from '../../transport/backends';
  import { userFacingError } from '../../utils/userFacingError';
  import { GHOST_BUTTON_CLASS, INPUT_CLASS, SECONDARY_BUTTON_CLASS, SELECT_CLASS } from './styles';
  import { trackerConnectionName } from './trackerConnectionStatus';

  let {
    backend,
    connections,
    listed,
    disabled,
    disabledReason,
    onAdd,
  }: {
    backend: BackendKey;
    /** The connections a board can come from: on, and not Tempo. */
    connections: readonly TrackerConnection[];
    /** `${connection}\u0000${board}` of every board the project lists. */
    listed: ReadonlySet<string>;
    disabled: boolean;
    disabledReason?: string;
    onAdd: (board: Omit<TrackerProjectBoard, 'opens_first'>) => Promise<boolean>;
  } = $props();

  const id = $props.id();

  type Load<T> = { state: 'idle' } | { state: 'loading' } | { state: 'done'; value: T } | { state: 'failed'; error: unknown };

  let open = $state(false);
  let connectionId = $state('');
  let projects = $state<Load<TrackerProjectView[]>>({ state: 'idle' });
  let filter = $state('');
  let project = $state<TrackerProjectView | null>(null);
  let boards = $state<Load<TrackerBoardInfo[]>>({ state: 'idle' });
  // Every read bumps it, so a reply for an earlier choice lands nowhere.
  let generation = 0;

  let shownProjects = $derived.by(() => {
    if (projects.state !== 'done') return [];
    const needle = filter.trim().toLowerCase();
    if (!needle) return projects.value;
    return projects.value.filter((p) => p.key.toLowerCase().includes(needle) || p.name.toLowerCase().includes(needle));
  });

  async function loadProjects(): Promise<void> {
    const mine = ++generation;
    const connection = connectionId;
    project = null;
    boards = { state: 'idle' };
    projects = { state: 'loading' };
    try {
      const value = await withBackendTarget(backend, () => TrackerProjects(connection));
      if (mine === generation) projects = { state: 'done', value };
    } catch (error) {
      if (mine === generation) projects = { state: 'failed', error };
    }
  }

  async function loadBoards(): Promise<void> {
    const chosen = project;
    if (!chosen) return;
    const mine = ++generation;
    const connection = connectionId;
    boards = { state: 'loading' };
    try {
      const value = await withBackendTarget(backend, () => TrackerBoards(connection, chosen.id));
      if (mine === generation) boards = { state: 'done', value };
    } catch (error) {
      if (mine === generation) boards = { state: 'failed', error };
    }
  }

  function start(): void {
    open = true;
    connectionId = connections.length === 1 ? connections[0].id : '';
    filter = '';
    if (connectionId) void loadProjects();
  }

  function close(): void {
    generation++;
    open = false;
    connectionId = '';
    projects = { state: 'idle' };
    project = null;
    boards = { state: 'idle' };
  }

  function chooseConnection(value: string): void {
    connectionId = value;
    filter = '';
    if (value) void loadProjects();
    else {
      generation++;
      projects = { state: 'idle' };
      project = null;
      boards = { state: 'idle' };
    }
  }

  function chooseProject(value: TrackerProjectView): void {
    project = value;
    void loadBoards();
  }

  async function add(board: TrackerBoardInfo): Promise<void> {
    if (!project) return;
    const added = await onAdd({
      connection: connectionId,
      board: board.id,
      name: board.name,
      tracker_project: project.key || project.name,
    });
    if (added) close();
  }
</script>

<div class="flex flex-col gap-2" data-testid="tracker-board-add">
  {#if !open}
    <div>
      <button
        type="button"
        class={SECONDARY_BUTTON_CLASS}
        disabled={disabled || connections.length === 0}
        title={disabledReason ?? (connections.length === 0 ? 'Add a Jira or Linear connection first, and turn it on.' : undefined)}
        onclick={start}
        data-testid="tracker-board-add-open"
      >Add a board</button>
    </div>
  {:else}
    <div class="flex flex-col gap-2.5 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1/40 p-3" data-testid="tracker-board-picker">
      <label class="flex flex-col gap-1 text-[0.75rem] text-fg" for="{id}-connection">
        Connection
        <select
          id="{id}-connection"
          class={SELECT_CLASS}
          value={connectionId}
          onchange={(e) => chooseConnection((e.target as HTMLSelectElement).value)}
          data-testid="tracker-board-add-connection"
        >
          <option value="" disabled>Choose a connection</option>
          {#each connections as connection (connection.id)}
            <option value={connection.id}>{trackerConnectionName(connection)}</option>
          {/each}
        </select>
      </label>

      {#if projects.state === 'loading'}
        <p class="text-[0.75rem] text-fg-muted" data-testid="tracker-board-add-projects-loading">Asking the tracker for its projects…</p>
      {:else if projects.state === 'failed'}
        <div class="flex flex-wrap items-center gap-2" role="alert">
          <p class="text-[0.75rem] text-error" data-testid="tracker-board-add-projects-error">{userFacingError(projects.error)}</p>
          <button type="button" class={GHOST_BUTTON_CLASS} onclick={() => void loadProjects()} data-testid="tracker-board-add-projects-retry">Try again</button>
        </div>
      {:else if projects.state === 'done' && !project}
        {#if projects.value.length === 0}
          <p class="text-[0.75rem] text-fg-muted">This account sees no projects on the tracker.</p>
        {:else}
          <input
            class={INPUT_CLASS}
            aria-label="Filter projects"
            placeholder="Filter projects"
            bind:value={filter}
            data-testid="tracker-board-add-filter"
          />
          <ul class="flex max-h-64 flex-col overflow-y-auto" data-testid="tracker-board-add-projects">
            {#each shownProjects as p (p.id)}
              <li>
                <button
                  type="button"
                  class="{GHOST_BUTTON_CLASS} w-full text-left text-fg"
                  onclick={() => chooseProject(p)}
                  data-testid="tracker-board-add-project"
                >
                  {#if p.key && p.key !== p.name}<span class="font-medium">{p.key}</span> · {/if}{p.name}
                </button>
              </li>
            {:else}
              <li class="px-2 py-1 text-[0.75rem] text-fg-muted">No project matches.</li>
            {/each}
          </ul>
        {/if}
      {/if}

      {#if project}
        <div class="flex flex-wrap items-center gap-2 text-[0.75rem] text-fg">
          <span>Boards of <span class="font-medium">{project.key || project.name}</span></span>
          <button type="button" class={GHOST_BUTTON_CLASS} onclick={() => { project = null; generation++; boards = { state: 'idle' }; }}>Another project</button>
        </div>
        {#if boards.state === 'loading'}
          <p class="text-[0.75rem] text-fg-muted" data-testid="tracker-board-add-boards-loading">Asking the tracker for the project's boards…</p>
        {:else if boards.state === 'failed'}
          <div class="flex flex-wrap items-center gap-2" role="alert">
            <p class="text-[0.75rem] text-error" data-testid="tracker-board-add-boards-error">{userFacingError(boards.error)}</p>
            <button type="button" class={GHOST_BUTTON_CLASS} onclick={() => void loadBoards()} data-testid="tracker-board-add-boards-retry">Try again</button>
          </div>
        {:else if boards.state === 'done'}
          {#if boards.value.length === 0}
            <p class="text-[0.75rem] text-fg-muted">This project has no boards.</p>
          {:else}
            <ul class="flex flex-col" data-testid="tracker-board-add-boards">
              {#each boards.value as board (board.id)}
                {@const already = listed.has(`${connectionId}\u0000${board.id}`)}
                <li>
                  <button
                    type="button"
                    class="{GHOST_BUTTON_CLASS} w-full text-left text-fg disabled:cursor-not-allowed disabled:opacity-50"
                    disabled={already || disabled}
                    title={already ? 'This project lists it already.' : disabledReason}
                    onclick={() => void add(board)}
                    data-testid="tracker-board-add-board"
                  >
                    {board.name}{#if already}<span class="text-fg-hint"> · listed</span>{/if}
                  </button>
                </li>
              {/each}
            </ul>
          {/if}
        {/if}
      {/if}

      <div>
        <button type="button" class={GHOST_BUTTON_CLASS} onclick={close}>Cancel</button>
      </div>
    </div>
  {/if}
</div>
