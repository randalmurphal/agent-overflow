<script lang="ts">
  // The ticket's Work (docs/specs/issues.md, "Ticket"), at the end of its
  // rail: the threads linked to it on every attached computer, from the
  // client's catalog, each with its state and who started it; the pull
  // requests and branches whose title or name holds its key, as its
  // computer reads them and its forge's search finds them, each failed
  // search in the Pull requests page's words (`stores/ticketWork.svelte.ts`); Ask and Start
  // work, and a Start work run's steps until it ends. Each row opens what
  // it names and has its menu on a right-click or long press: a pull
  // request its review in the ticket's place when the ticket's computer
  // names a project that can show it, else its page on the forge.
  import { untrack } from 'svelte';
  import Button from '../primitives/Button.svelte';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import StartWorkForm from './StartWorkForm.svelte';
  import type { TicketBranch, TicketPullRequest } from '../../stores/bindings';
  import { assistantCurrentThread, getThreadById, getThreads } from '../../stores/threads.svelte';
  import { assistantDisplayName } from '../../stores/assistants.svelte';
  import { attachedBackendEntry, backendDisplayName, hasMultipleBackends, unreachableThreadMachineName } from '../../stores/attachedBackends.svelte';
  import { getEffectiveThreadStatus, getThreadWaitingOn } from '../../stores/threadStatuses.svelte';
  import { removeThreadTicketLink } from '../../stores/threadTicketLinks.svelte';
  import { getProjectLabelText } from '../../stores/projects.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { TrackerTicketAttachment } from '../../stores/trackerTicket.svelte';
  import type { TicketWriteContext } from '../../stores/trackerTicketWrites.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import {
    askAboutTicket,
    dismissStartWorkRun,
    keyPrefix,
    loadTicketWork,
    openTicketThread,
    retryStartWork,
    startWork,
    startWorkRun,
    stepRetryable,
    ticketAskPending,
    ticketWork,
    type StartWorkPlan,
    type StartWorkStep,
    type StepActivity,
    type StepOutcome,
  } from '../../stores/ticketWork.svelte';
  import { threadBackend } from '../../transport/entityIndex';
  import { workGate, workProject, workProjects } from './ticketWorkGate';
  import { SELECT_CLASS } from '../settings/styles';
  import type { Thread } from '../../types/models';
  import { copyToClipboard } from '../../utils/clipboard';
  import { ciStatusDotClass, ciStatusText } from '../../utils/ciStatus';
  import { pathBasename } from '../../utils/pathDisplay';
  import { lastKnownThreadStatusPill, resolveThreadStatusPill } from '../../utils/threadStatusPill';
  import { userFacingError } from '../../utils/userFacingError';
  import { pullNumberLabel } from '../pulls/pullRowView';
  import PullsProjectError from '../pulls/PullsProjectError.svelte';
  import { forgeFailureFrom, forgeReadFailureCause, rateLimitRetryMessage } from '../../utils/forgeFailure';
  import { prRefFromWire } from '../../utils/prReference';
  import type { TicketPullRequestOpen } from '../../stores/issuesPane.svelte';
  import { openPullOnForge } from './ticketPullOnForge';

  interface Props {
    attachment: TrackerTicketAttachment;
    /** The ticket's project, as the pane holds it (`TicketTrail.
     * project`): the thread of an Ask or Start work is made in it,
     * and its setup holds the Start work defaults. Null when none is
     * known. */
    project: string | null;
    /** The person picked another project for the ticket. */
    onProject: (project: string) => void;
    /** The person's account on the connection; null when it was never
     * verified. */
    me: string | null;
    /** The open shown asked for its Start work form
     * (`TicketTrail.startWorkAsked`). */
    startWorkAsked: boolean;
    /** Work took that ask. */
    onStartWorkTaken: () => void;
    /** Shows a pull request in the ticket's place. */
    onOpenPullRequest: (pr: TicketPullRequestOpen) => void;
  }

  let { attachment, project: ticketProject, onProject, me, startWorkAsked, onStartWorkTaken, onOpenPullRequest }: Props = $props();

  const ref = $derived(attachment.ref);
  const ticketState = $derived(attachment.current);
  const detail = $derived(ticketState?.detail ?? null);
  const card = $derived(detail?.card ?? null);
  const ctx: TicketWriteContext = $derived({ key: attachment.key, ref: attachment.ref, label: card?.key || 'the ticket' });

  const projects = $derived(workProjects(ref.backend, ticketProject));
  const project = $derived(workProject(projects, ticketProject));
  // The ticket's tracker as its subscription answered it; null until then.
  const tracker = $derived(attachment.tracker);
  const place = $derived(tracker && card ? { tracker: tracker.kind, site: tracker.site, project: keyPrefix(card.key) } : null);

  // The threads linked to this ticket: its site and issue, on any computer.
  const threads = $derived(tracker
    ? getThreads().filter((t) => t.ticketLink && t.ticketLink.kind === tracker.kind && t.ticketLink.site === tracker.site && t.ticketLink.issueId === ref.issue)
    : []);
  const started = $derived(threads.find((t) => t.ticketLink?.origin === 'start_work') ?? null);
  const several = $derived(hasMultipleBackends());

  const read = $derived(ticketWork(attachment.key));
  const work = $derived(read?.work ?? null);

  // Read once the ticket's computer has read the ticket (its key is what
  // Work matches, and the computer answers nothing before), and again
  // after each Refresh of the ticket.
  let wasRefreshing = false;
  $effect(() => {
    const refreshing = ticketState?.refreshing ?? false;
    const ended = wasRefreshing && !refreshing;
    wasRefreshing = refreshing;
    if (ended) untrack(() => loadTicketWork(attachment.key, attachment.ref));
  });
  const readable = $derived(detail !== null);
  $effect(() => {
    const { key, ref: at } = attachment;
    if (readable) untrack(() => loadTicketWork(key, at));
  });

  function owner(t: Thread): string {
    const spawner = t.spawnedByThreadId ? getThreadById(t.spawnedByThreadId) : undefined;
    if (spawner) return `Started by ${spawner.agentId ? assistantDisplayName(spawner) : spawner.title || 'another thread'}`;
    const assistant = t.helperOfAgentId ? assistantCurrentThread(t.helperOfAgentId) : undefined;
    if (assistant) return `Helper of ${assistantDisplayName(assistant)}`;
    return '';
  }

  function computerOf(t: Thread): string {
    const backend = threadBackend(t.id);
    const entry = backend ? attachedBackendEntry(backend) : undefined;
    return entry ? backendDisplayName(entry) : '';
  }

  /** The thread in the worktree that has the branch checked out, of the
   * branch's own project, so a thread on another computer with the same
   * layout never answers for it. */
  function threadOf(b: TicketBranch): Thread | null {
    if (!b.worktreePath) return null;
    return getThreads().find((t) => t.projectId === b.projectId && t.worktreePath === b.worktreePath) ?? null;
  }

  const PR_STATE: Readonly<Record<string, string>> = { draft: 'Draft', open: 'Open', merged: 'Merged', closed: 'Closed' };

  function openOnForge(pr: TicketPullRequest): Promise<void> {
    return openPullOnForge(pr.ref.Forge, pr.ref.Number, pr.url);
  }

  // The review a row opens in the ticket's place: the computer names the
  // project whose checkout can show it (`reviewProjectId`), else none and
  // the row opens on the forge.
  function reviewOf(pr: TicketPullRequest): TicketPullRequestOpen | null {
    const ref = pr.reviewProjectId ? prRefFromWire(pr.ref) : null;
    return ref && pr.reviewProjectId ? { pr: ref, projectId: pr.reviewProjectId, url: pr.url, title: pr.title || pr.branch } : null;
  }

  function openPull(pr: TicketPullRequest): void {
    const review = reviewOf(pr);
    if (review) onOpenPullRequest(review);
    else void openOnForge(pr);
  }

  async function copy(text: string, what: string): Promise<void> {
    if (!(await copyToClipboard(text))) addToast('error', `Could not copy the ${what}.`);
  }

  // Start work: the form, then the run.
  let startButton: HTMLElement | undefined = $state();
  let formOpen = $state(false);
  let formBranch = $state<{ readonly name: string; readonly projectId: string } | null>(null);
  const run = $derived(startWorkRun(attachment.key));

  function openForm(branch: { readonly name: string; readonly projectId: string } | null): void {
    formBranch = branch;
    formOpen = true;
  }

  function begin(plan: StartWorkPlan): void {
    formOpen = false;
    void startWork(ctx, plan, () => attachment.current);
  }

  const STEP_NAMES: Readonly<Record<StartWorkStep, string>> = {
    thread: 'Make the thread',
    move: 'Move the ticket',
    assign: 'Assign it to me',
    defaults: 'Save the defaults',
  };

  // A step that was a tracker write: its row is no control of the write's
  // target. The Status and Assignee fields are, and say what became of the
  // write by the store's rule; the row names the kind and shows the write
  // its run sent while the store keeps its outcome (TicketWriteNote's
  // `only`).

  const DOING: Readonly<Record<StepActivity, string>> = { reading: 'Reading the ticket…', saving: 'Saving…', making: 'Making the thread…' };

  function stepText(o: StepOutcome): string {
    if ('written' in o && o.written) {
      switch (o.kind) {
        case 'applied': return 'Done.';
        case 'refused': return 'Refused.';
        case 'unknown': return 'Not known.';
        case 'not_sent': return 'Not sent.';
        case 'failed': return 'Failed.';
      }
    }
    switch (o.kind) {
      case 'waiting': return 'Waiting.';
      case 'running': return DOING[o.doing];
      case 'skipped': return '';
      case 'applied': return o.note ? `Done. ${o.note}` : 'Done.';
      case 'already': return o.text;
      case 'refused': return `Refused: ${o.text}`;
      case 'unknown': return `Not known: ${o.text}`;
      case 'not_sent': return `Not sent: ${o.text}`;
      case 'failed': return `Failed: ${o.text}`;
    }
  }

  const STEP_TONE: Readonly<Record<StepOutcome['kind'], string>> = {
    waiting: 'text-fg-muted', running: 'text-fg-muted', skipped: 'text-fg-muted', applied: 'text-fg', already: 'text-fg',
    refused: 'text-error', unknown: 'text-warning', not_sent: 'text-warning', failed: 'text-error',
  };

  type Menu =
    | { kind: 'thread'; thread: Thread }
    | { kind: 'pr'; pr: TicketPullRequest }
    | { kind: 'branch'; branch: TicketBranch };
  let menu = $state<{ x: number; y: number; of: Menu } | null>(null);
  function openMenu(event: MouseEvent, of: Menu): void {
    event.preventDefault();
    menu = { x: event.clientX, y: event.clientY, of };
  }
  // The action reads the row the menu names, so it runs before the menu
  // closes.
  function act(action: () => void | Promise<void>): void {
    void action();
    menu = null;
  }

  const asking = $derived(ticketAskPending(attachment.key));
  const gate = $derived(workGate(ref.backend, project));
  const askBlocked = $derived(gate.ask);
  const grantNote = $derived(gate.note);
  // Why Start work cannot run now, '' when it can: the one answer its
  // button, a branch's row and menu, and a Start work asked for go by.
  const startBlocked = $derived(gate.start || (run?.running ? `Start work is already running on ${card?.key || 'this ticket'}.` : ''));

  // The open shown asked for its Start work form (a Board card's Start
  // work): taken once the ticket is read, and the form opened when Start
  // work can run now. When it cannot, Work shows why: the run going on,
  // or the button whose title says it.
  let runBox: HTMLElement | undefined = $state();
  $effect(() => {
    if (!startWorkAsked || !detail) return;
    untrack(() => {
      onStartWorkTaken();
      (run?.running ? runBox : startButton)?.scrollIntoView({ block: 'nearest' });
      if (!startBlocked) openForm(null);
    });
  });
</script>

<section class="mt-4 flex flex-col gap-2 border-t border-border-subtle pt-3" data-testid="ticket-work">
  <div class="flex items-center gap-2">
    <span class="text-sm font-medium text-fg">Work</span>
    <span class="flex-1"></span>
    <Button size="xs" variant="secondary" disabled={!detail || !!askBlocked || asking} loading={asking} title={askBlocked || 'A thread with this ticket attached; nothing on the ticket changes'} testId="ticket-work-ask" onclick={() => { void askAboutTicket(ctx, project); }}>Ask</Button>
    <span bind:this={startButton} class="inline-flex">
      <Button size="xs" variant="primary" disabled={!detail || !!startBlocked} title={startBlocked || undefined} testId="ticket-work-start" onclick={() => openForm(null)}>Start work</Button>
    </span>
  </div>
  <label class="flex items-center gap-2 px-1 text-xs text-fg-muted">
    <span class="shrink-0">Threads go in</span>
    <select class="{SELECT_CLASS} min-w-0 flex-1" value={project} onchange={(e) => onProject((e.target as HTMLSelectElement).value)} data-testid="ticket-work-project">
      {#if !project}<option value="" disabled>Pick a project</option>{/if}
      {#each projects as p (p.id)}
        <option value={p.id}>{getProjectLabelText(p.id)}</option>
      {/each}
    </select>
  </label>
  {#if grantNote}
    <p class="px-1 text-xs text-fg-muted" data-testid="ticket-work-grants">{grantNote}</p>
  {/if}

  {#if run}
    <div class="flex flex-col gap-1 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-1.5 text-xs" bind:this={runBox} data-testid="start-work-run" data-running={run.running ? '' : undefined}>
      {#each Object.entries(run.steps) as [step, outcome] (step)}
        {#if outcome.kind !== 'skipped'}
          {@const written = 'written' in outcome ? outcome.written : undefined}
          <div class="flex flex-col" data-testid="start-work-step" data-step={step} data-outcome={outcome.kind}>
            <span class="font-medium text-fg">{STEP_NAMES[step as StartWorkStep]}{step === 'move' && run.plan.status ? ` to ${run.plan.status.name}` : ''}{step === 'thread' ? ` in ${getProjectLabelText(run.plan.repository)} on ${run.plan.branch}` : ''}</span>
            <span class={STEP_TONE[outcome.kind]}>{stepText(outcome)}</span>
            {#if written}
              <TicketWriteNote ticketKey={attachment.key} target={written.target} fieldName={(id) => id} only={written.outcome} />
            {/if}
          </div>
        {/if}
      {/each}
      {#if !run.running}
        <div class="flex flex-wrap justify-end gap-1.5 pt-1">
          {#if run.thread}
            {@const made = run.thread}
            <Button size="xs" variant="ghost" testId="start-work-open-thread" onclick={() => { void openTicketThread(made); }}>Open thread</Button>
          {/if}
          {#if Object.values(run.steps).some(stepRetryable)}
            <Button size="xs" variant="secondary" testId="start-work-retry" onclick={() => { void retryStartWork(ctx, () => attachment.current); }}>Try again</Button>
          {/if}
          <Button size="xs" variant="ghost" testId="start-work-dismiss" onclick={() => dismissStartWorkRun(attachment.key)}>Dismiss</Button>
        </div>
      {/if}
    </div>
  {/if}

  <div class="flex flex-col" data-testid="ticket-work-threads">
    <span class="px-1 text-xs text-fg-muted">Threads</span>
    {#each threads as t (t.id)}
      {@const machine = unreachableThreadMachineName(t.id, t.projectId)}
      {@const resolved = resolveThreadStatusPill(t, getEffectiveThreadStatus(t), getThreadWaitingOn(t.id))}
      {@const pill = machine !== null ? lastKnownThreadStatusPill(resolved, machine) : resolved}
      {@const by = owner(t)}
      <button
        type="button"
        class="flex min-w-0 flex-col rounded-[var(--radius-control)] px-1 py-0.5 text-left hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:min-h-9 compact:select-none"
        data-testid="ticket-work-thread"
        data-thread={t.id}
        onclick={() => { void openTicketThread(t); }}
        oncontextmenu={(e) => openMenu(e, { kind: 'thread', thread: t })}
      >
        <span class="flex min-w-0 items-center gap-1.5 text-sm text-fg">
          <span class="h-2 w-2 shrink-0 rounded-full {pill ? `${pill.dotClass} ${pill.pulse ? 'animate-pulse' : ''}` : 'bg-border-subtle'}" aria-hidden="true"></span>
          <span class="min-w-0 truncate">{t.title || 'Untitled thread'}</span>
        </span>
        <span class="truncate pl-3.5 text-xs text-fg-muted" data-testid="ticket-work-thread-detail">{[pill?.label ?? 'Idle', by, several ? computerOf(t) : ''].filter(Boolean).join(' · ')}</span>
      </button>
    {:else}
      <p class="px-1 text-xs text-fg-muted">No thread is linked to this ticket.</p>
    {/each}
  </div>

  <div class="flex flex-col" data-testid="ticket-work-pulls">
    <span class="px-1 text-xs text-fg-muted">Pull requests</span>
    {#each work?.pullRequests ?? [] as pr (pr.url)}
      {@const here = !!reviewOf(pr)}
      <button
        type="button"
        class="flex min-w-0 flex-col rounded-[var(--radius-control)] px-1 py-0.5 text-left hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:min-h-9 compact:select-none"
        title="Open {pullNumberLabel(pr.ref.Forge, pr.ref.Number)} {here ? 'here' : 'on the forge'}"
        data-testid="ticket-work-pull"
        data-state={pr.state}
        data-opens={here ? 'review' : 'forge'}
        onclick={() => openPull(pr)}
        oncontextmenu={(e) => openMenu(e, { kind: 'pr', pr })}
      >
        <span class="flex min-w-0 items-center gap-1.5 text-sm text-fg">
          <span class="shrink-0 tabular-nums text-fg-muted">{pullNumberLabel(pr.ref.Forge, pr.ref.Number)}</span>
          <span class="min-w-0 truncate">{pr.title || pr.branch}</span>
        </span>
        <span class="flex min-w-0 items-center gap-1.5 truncate text-xs text-fg-muted">
          <span data-testid="ticket-work-pull-state">{PR_STATE[pr.state] ?? pr.state}</span>
          {#if pr.author}<span>· {pr.author}</span>{/if}
          {#if pr.ci}
            <span class="inline-flex items-center gap-1">· <span class="h-1.5 w-1.5 rounded-full {ciStatusDotClass(pr.ci)}" aria-hidden="true"></span>CI {ciStatusText(pr.ci)}</span>
          {/if}
        </span>
      </button>
    {:else}
      <p class="px-1 text-xs text-fg-muted">{read?.loading && !work ? 'Reading…' : 'No pull request names this ticket.'}</p>
    {/each}
    {#if read?.searching && !read.loading}
      <p class="px-1 text-xs text-fg-muted" data-testid="ticket-work-searching">Searching the forge…</p>
    {/if}
    {#each work?.searchFull ?? [] as repo (`${repo.Host}\n${repo.Namespace}/${repo.Repo}`)}
      <p class="px-1 text-xs text-fg-muted" data-testid="ticket-work-search-full">The search of {repo.Namespace}/{repo.Repo} stopped after 500 matches, so these may not be all of them.</p>
    {/each}
    {#each work?.forgeProblems ?? [] as problem (`${problem.projectId}\n${problem.repo.Namespace}/${problem.repo.Repo}`)}
      {@const failure = forgeFailureFrom(problem)}
      {@const what = problem.repo.Repo ? `${problem.repo.Namespace}/${problem.repo.Repo}` : getProjectLabelText(problem.projectId)}
      <!-- A repository's search may have read rows before it failed; a
        project whose origin git could not read was not searched. -->
      <div data-testid="ticket-work-forge-problem">
        <PullsProjectError
          message={failure?.kind === 'rate_limited' ? rateLimitRetryMessage(problem.repo.Forge, what, failure) : problem.repo.Repo ? `Could not finish searching ${what}: ${problem.error}` : `Could not search ${what}: ${problem.error}`}
          {failure}
          polled={false}
          forge={problem.repo.Forge}
          cause={forgeReadFailureCause(problem, ref.backend)}
          onRetry={() => { void loadTicketWork(attachment.key, attachment.ref); }}
          retrying={read?.searching ?? false}
        />
      </div>
    {/each}
    {#if read?.searchError && !read.error}
      <p class="px-1 text-xs text-error" role="alert" data-testid="ticket-work-search-error">Could not search the forge: {userFacingError(read.searchError)}</p>
    {/if}
  </div>

  <div class="flex flex-col" data-testid="ticket-work-branches">
    <span class="px-1 text-xs text-fg-muted">Branches</span>
    {#each work?.branches ?? [] as b (`${b.projectId}\n${b.name}`)}
      {@const holder = threadOf(b)}
      {@const where = b.worktreePath ? (holder ? `in ${holder.title || 'a thread'}` : `checked out at ${pathBasename(b.worktreePath)}`) : ''}
      {@const drift = [b.aheadCount ? `${b.aheadCount} ahead` : '', b.behindCount ? `${b.behindCount} behind` : ''].filter(Boolean).join(', ')}
      {@const startable = !!detail && !startBlocked}
      <button
        type="button"
        class="flex min-w-0 flex-col rounded-[var(--radius-control)] px-1 py-0.5 text-left hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-default disabled:hover:bg-transparent compact:min-h-9 compact:select-none"
        title={holder ? `Open ${holder.title || 'its thread'}` : (startable ? 'Start work on this branch' : startBlocked || undefined)}
        aria-disabled={holder || startable ? undefined : 'true'}
        data-testid="ticket-work-branch"
        onclick={() => {
          if (holder) void openTicketThread(holder);
          else if (startable) openForm({ name: b.name, projectId: b.projectId });
        }}
        oncontextmenu={(e) => openMenu(e, { kind: 'branch', branch: b })}
      >
        <span class="min-w-0 truncate font-mono text-xs text-fg">{b.name}</span>
        <span class="truncate text-xs text-fg-muted">{[getProjectLabelText(b.projectId), where, drift].filter(Boolean).join(' · ')}</span>
      </button>
    {:else}
      <p class="px-1 text-xs text-fg-muted">{read?.loading && !work ? 'Reading…' : 'No branch names this ticket.'}</p>
    {/each}
    {#each work?.problems ?? [] as problem (problem.projectId)}
      <p class="px-1 text-xs text-error" role="alert" data-testid="ticket-work-problem">Could not read the branches of {getProjectLabelText(problem.projectId)}: {problem.error}</p>
    {/each}
  </div>

  {#if read?.denied}
    <p class="px-1 text-xs text-fg-muted" data-testid="ticket-work-denied">This device may not read git on this computer, so Work lists no pull requests or branches.</p>
  {:else if read?.error}
    <p class="px-1 text-xs text-error" role="alert" data-testid="ticket-work-error">Could not read the ticket's work: {userFacingError(read.error)}</p>
  {/if}
</section>

{#if detail && project}
  <StartWorkForm
    anchor={startButton}
    open={formOpen}
    {detail}
    {projects}
    {project}
    {place}
    me={me ?? ''}
    branches={work?.branches ?? []}
    {started}
    branch={formBranch}
    onStart={begin}
    onOpenThread={(t) => { formOpen = false; void openTicketThread(t); }}
    onClose={() => { formOpen = false; }}
  />
{/if}

{#if menu}
  {@const of = menu.of}
  <ContextMenu x={menu.x} y={menu.y} ariaLabel="Work actions" onDismiss={() => { menu = null; }}>
    {#if of.kind === 'thread'}
      <MenuItem label="Open" testId="ticket-work-menu-open" onSelect={() => act(() => openTicketThread(of.thread))} />
      <MenuDivider />
      <MenuItem label="Remove link" testId="ticket-work-menu-unlink" onSelect={() => act(() => removeThreadTicketLink(of.thread))} />
    {:else if of.kind === 'pr'}
      <MenuItem label="Open on the forge" testId="ticket-work-menu-forge" onSelect={() => act(() => openOnForge(of.pr))} />
      <MenuDivider />
      <MenuItem label="Copy link" testId="ticket-work-menu-copy-link" onSelect={() => act(() => copy(of.pr.url, 'link'))} />
    {:else}
      {@const holder = threadOf(of.branch)}
      {#if holder}
        <MenuItem label="Open its thread" testId="ticket-work-menu-branch-thread" onSelect={() => act(() => openTicketThread(holder))} />
        <MenuDivider />
      {/if}
      <MenuItem label="Copy name" testId="ticket-work-menu-copy-branch" onSelect={() => act(() => copy(of.branch.name, 'branch name'))} />
      <MenuDivider />
      <MenuItem label="Start work on this branch" disabled={!detail || !!startBlocked} description={startBlocked || undefined} testId="ticket-work-menu-start-branch" onSelect={() => act(() => openForm({ name: of.branch.name, projectId: of.branch.projectId }))} />
    {/if}
  </ContextMenu>
{/if}
