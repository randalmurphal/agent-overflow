<script lang="ts">
  // The Credit group of Settings > Issues > Points and credit
  // (docs/specs/issues.md, "Credit"): the project's credit section
  // (trackerapp.Credit). A switch turns counting credit on and off; off
  // keeps every rule and pick. Review credit is a share of a finished
  // ticket's points to each reviewer who did at least a number of the
  // chosen kinds of action; support credit is a point for every so many
  // hours logged on a support ticket; and which tickets are support is
  // picked for each Jira site of the project's boards by type, label and
  // board. A rule's change counts from this sprint, kept beside the rule
  // finished sprints were counted with, or for every sprint (`every`, the
  // page's choice, not saved). Wording follows the unit each read board
  // is counted in (BoardPace.counts). The types to pick are the boards'
  // projects' issue types, every type a person who can see the project
  // reads (stores/trackerTicketWrites.svelte.ts), read once credit is on.
  import type { BoardPace, CreditRules, SiteSupport } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { tick } from 'svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { isLocalEnter } from '../../utils/localEnter';
  import { settingsComputerName } from '../../stores/settings.svelte';
  import { countsTickets, creditOn, withCreditOn, withCreditRules, withSupport, type CreditChange, type SupportChange } from '../../stores/trackerSetup.svelte';
  import { boardProjects, boardWriteContext, loadBoardProjects, loadProjectIssueTypes, projectIssueTypes } from '../../stores/trackerTicketWrites.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import Segmented from '../primitives/Segmented.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import type { TrackerProjectPage } from './TrackerProjectSettings.svelte';
  import { settingsComputer } from './settingsComputer';
  import { CHIP_BASE_CLASS, CHIP_RESTING_CLASS, CHIP_SELECTED_CLASS, GHOST_BUTTON_CLASS, INPUT_CLASS, NUMBER_CLASS } from './styles';

  interface Props {
    page: TrackerProjectPage;
    /** The project's Jira boards, from the page's one read. */
    boards: readonly BoardPace[];
  }

  let { page, boards }: Props = $props();

  const { backend } = settingsComputer();
  const compact = $derived(isCompactLayout());

  /** The kinds of review action, in the order the computer keeps them
   * (trackerapp's creditActions). */
  const ACTIONS: readonly { id: string; name: string }[] = [
    { id: 'commented', name: 'Commented' },
    { id: 'approved', name: 'Approved' },
    { id: 'logged_time', name: 'Logged time' },
  ];

  const credit = $derived(page.setup.credit ?? null);
  const on = $derived(creditOn(page.setup));
  const newest = $derived(credit ? credit.rules[credit.rules.length - 1] : null);
  const locked = $derived(page.lockReason !== '');
  // The unit each board the mirror has read is counted in; before any is
  // read, the project's own pick.
  const readBoards = $derived(boards.filter((b) => b.read));
  const tickets = $derived(readBoards.length > 0 ? readBoards.every((b) => b.counts === 'none') : countsTickets(page.setup));
  // The boards that count tickets beside others that count points.
  const ticketBoards = $derived(tickets ? [] : readBoards.filter((b) => b.counts === 'none'));
  // A board estimated in time counts its support hours as they are,
  // whatever unit the project counts it in (trackerapp's ProjectSupport).
  const timeBoards = $derived(readBoards.some((b) => b.estimate === 'time'));

  let every = $state<'from' | 'every'>('from');

  // An edit is what the person chose when they acted: its value and its
  // scope are read then, and the save makes it in the setup it builds on,
  // which may be read again first (TrackerProjectSettings' save), with
  // nothing read from the page's controls meanwhile.
  function rule(change: CreditChange): Promise<boolean> {
    const everySprint = every === 'every';
    return page.save((held) => withCreditRules(held, change, everySprint));
  }

  function boardNames(list: readonly BoardPace[]): string {
    const names = list.map((b) => b.name || `Board ${b.board}`);
    return names.length < 2 ? names.join('') : `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`;
  }

  function actionsText(r: CreditRules): string {
    return ACTIONS.filter((a) => r.review_actions.includes(a.id)).map((a) => a.name.toLowerCase()).join(', ');
  }

  /** Turns review action `id` on when the newest rule leaves it out, else
   * off: in every snapshot it is made in, the action ends as chosen. */
  function toggleAction(id: string): void {
    void rule({ action: id, on: !newest!.review_actions.includes(id) });
  }

  type NumberField = 'review_share' | 'review_need' | 'hours_per_point';
  /** The number field whose last value was not a whole number, and what
   * it says, until the next value of a number field. */
  let wholeNote = $state<{ field: NumberField; text: string } | null>(null);

  /** Saves a number field's value; the field then shows what is stored,
   * since neither a value that is not a whole number nor a refused or
   * failed save moves the value the field is given. */
  async function setNumber(input: HTMLInputElement, field: NumberField, name: string): Promise<void> {
    const n = input.valueAsNumber;
    const sent = input.value;
    if (Number.isInteger(n)) {
      wholeNote = null;
      await rule({ [field]: n } as CreditChange);
      await tick();
    } else {
      wholeNote = { field, text: `${name} takes a whole number.` };
    }
    // A value typed since is the person's next edit, kept.
    if (newest && input.isConnected && input.value === sent) input.value = String(newest[field]);
  }

  // Each Jira site of the project's boards, with its boards, in the
  // boards' order.
  const sites = $derived.by(() => {
    const out: { site: string; boards: BoardPace[] }[] = [];
    for (const b of boards) {
      const found = out.find((s) => s.site === b.site);
      if (found) found.boards.push(b);
      else out.push({ site: b.site ?? '', boards: [b] });
    }
    return out;
  });

  function supportOf(site: string): SiteSupport {
    return credit?.support.find((s) => s.tracker === 'jira' && s.site === site) ?? { tracker: 'jira', site, types: [], labels: [], boards: [] };
  }

  function editSupport(site: string, change: SupportChange): void {
    void page.save((held) => withSupport(held, 'jira', site, change));
  }

  // The types to pick: each board's projects' issue types, read once
  // credit is on.
  $effect(() => {
    if (!on) return;
    for (const b of boards) void loadBoardProjects(boardWriteContext({ backend, connection: b.connection, board: b.board }));
  });
  $effect(() => {
    if (!on) return;
    for (const b of boards) {
      for (const p of boardProjects(boardWriteContext({ backend, connection: b.connection, board: b.board }))?.value ?? []) {
        void loadProjectIssueTypes(backend, b.connection, p.id);
      }
    }
  });

  /** A read of the types that failed: what it was of, why, and its
   * retry. */
  interface TypesFailure {
    readonly key: string;
    readonly text: string;
    readonly retry: () => void;
  }

  /** The types of a site's boards' projects, by ID, in the order read;
   * whether a read is out; and each read that failed. */
  function typesOf(site: { boards: BoardPace[] }): { types: { id: string; name: string }[]; reading: boolean; failed: TypesFailure[] } {
    const types = new Map<string, string>();
    let reading = false;
    const failed: TypesFailure[] = [];
    for (const b of site.boards) {
      const context = boardWriteContext({ backend, connection: b.connection, board: b.board });
      const projects = boardProjects(context);
      if (!projects?.value) reading ||= !projects || projects.loading;
      if (projects?.error) {
        failed.push({ key: `board ${b.connection}/${b.board}`, text: `The projects of ${boardNames([b])} could not be read: ${userFacingError(projects.error)}`,
          retry: () => void loadBoardProjects(context) });
      }
      for (const p of projects?.value ?? []) {
        const read = projectIssueTypes(backend, b.connection, p.id);
        if (!read?.value) reading ||= !read || read.loading;
        if (read?.error) {
          failed.push({ key: `project ${b.connection}/${p.id}`, text: `The issue types of ${p.key || p.name || `project ${p.id}`} could not be read: ${userFacingError(read.error)}`,
            retry: () => void loadProjectIssueTypes(backend, b.connection, p.id) });
        }
        for (const t of read?.value ?? []) if (!types.has(t.id)) types.set(t.id, t.name || `Type ${t.id}`);
      }
    }
    return { types: [...types].map(([id, name]) => ({ id, name })), reading, failed: failed.filter((f, i) => failed.findIndex((g) => g.key === f.key) === i) };
  }

  let labelDraft = $state<Record<string, string>>({});

  function addLabel(site: string): void {
    const label = (labelDraft[site] ?? '').trim();
    if (!label) return;
    labelDraft[site] = '';
    if (supportOf(site).labels.includes(label)) return;
    editSupport(site, { list: 'labels', id: label, on: true });
  }

  const unit = $derived(tickets ? 'ticket' : 'point');
</script>

<div class="flex flex-col gap-3" data-testid="tracker-credit">
  <p class="text-[0.8125rem] font-medium text-fg">Credit</p>
  {#if page.view.newer?.includes('credit') || !credit || !newest}
    <p class="text-[0.75rem] text-warning" data-testid="tracker-credit-newer">
      A newer version of Agent Overflow saved this project's credit rules. They are kept as they are; update the app on {settingsComputerName(backend) || 'this computer'} to change them here.
    </p>
  {:else}
    {@const r = newest}
    <ul class="flex flex-col gap-3">
      <li class="flex min-w-0 items-center gap-3 {compact ? 'min-h-9' : ''}" data-testid="tracker-credit-on">
        <div class="flex min-w-0 flex-1 flex-col">
          <span class="text-[0.8125rem] text-fg">Count credit</span>
          <span class="text-[0.71875rem] text-fg-hint">Reviews and hours on support tickets count toward each person's {unit}s. Turned off, the rules below are kept.</span>
        </div>
        <ToggleSwitch checked={on} ariaLabel="Count credit" disabled={locked} title={page.lockReason || undefined} onToggle={(next) => void page.save((held) => withCreditOn(held, next))} />
      </li>
      {#if on}
        <li class="flex min-w-0 flex-col gap-1.5" data-testid="tracker-credit-review">
          <span class="text-[0.8125rem] text-fg">Review credit</span>
          <span class="text-[0.71875rem] text-fg-hint" data-testid="tracker-credit-review-line">
            {r.review_share}% of {tickets ? 'each finished ticket' : `a finished ticket's ${timeBoards ? 'estimate' : 'points'}`}, to each reviewer who did at least {r.review_need} of: {actionsText(r)}. On the ticket or its pull request; the assignee never counts.
          </span>
          <div class="flex flex-wrap items-center gap-2 text-[0.75rem] text-fg-muted">
            <input
              type="number"
              class={NUMBER_CLASS}
              aria-label="Review share, percent"
              min="0"
              max="100"
              step="5"
              value={r.review_share}
              disabled={locked}
              onchange={(e) => void setNumber(e.currentTarget, 'review_share', 'The review share')}
              data-testid="tracker-credit-share"
            />
            <span>% to each reviewer who did at least</span>
            <input
              type="number"
              class={NUMBER_CLASS}
              aria-label="Kinds of review action needed"
              min="1"
              max={r.review_actions.length}
              step="1"
              value={r.review_need}
              disabled={locked}
              onchange={(e) => void setNumber(e.currentTarget, 'review_need', 'The kinds of review action needed')}
              data-testid="tracker-credit-need"
            />
            <span>of</span>
            {#each ACTIONS as a (a.id)}
              {@const chosen = r.review_actions.includes(a.id)}
              {@const last = chosen && r.review_actions.length === 1}
              <button
                type="button"
                class="{CHIP_BASE_CLASS} {chosen ? CHIP_SELECTED_CLASS : CHIP_RESTING_CLASS}"
                aria-pressed={chosen}
                disabled={locked || last}
                title={last ? 'A reviewer is counted by at least one kind of action.' : page.lockReason || undefined}
                onclick={() => toggleAction(a.id)}
                data-testid="tracker-credit-action"
                data-action={a.id}
              >{a.name}</button>
            {/each}
          </div>
          {#if wholeNote && wholeNote.field !== 'hours_per_point'}
            <p class="text-[0.71875rem] text-error" role="alert" data-testid="tracker-credit-whole">{wholeNote.text}</p>
          {/if}
        </li>
        <li class="flex min-w-0 flex-col gap-1.5" data-testid="tracker-credit-support">
          <span class="text-[0.8125rem] text-fg">Support credit</span>
          <span class="text-[0.71875rem] text-fg-hint" data-testid="tracker-credit-support-line">
            1 {unit} for every {r.hours_per_point} hours logged on support tickets, which carry no {tickets ? 'estimate' : 'points'}.{#if timeBoards}{' '}On a board estimated in time, the hours count as they are.{/if}{#if ticketBoards.length > 0}{' '}On {boardNames(ticketBoards)}, which {ticketBoards.length === 1 ? 'counts' : 'count'} tickets, a point is a ticket.{/if}
          </span>
          <div class="flex flex-wrap items-center gap-2 text-[0.75rem] text-fg-muted">
            <span>1 {unit} for every</span>
            <input
              type="number"
              class={NUMBER_CLASS}
              aria-label="Hours of support for 1 {unit}"
              min="1"
              max="16"
              step="1"
              value={r.hours_per_point}
              disabled={locked}
              onchange={(e) => void setNumber(e.currentTarget, 'hours_per_point', `The hours of support for 1 ${unit}`)}
              data-testid="tracker-credit-hours"
            />
            <span>hours</span>
          </div>
          {#if wholeNote?.field === 'hours_per_point'}
            <p class="text-[0.71875rem] text-error" role="alert" data-testid="tracker-credit-whole">{wholeNote.text}</p>
          {/if}
        </li>
        <li class="flex min-w-0 flex-col gap-1.5" data-testid="tracker-credit-counts-from">
          <div class="flex flex-wrap items-center gap-2">
            <span class="text-[0.8125rem] text-fg">A change counts from</span>
            <Segmented
              ariaLabel="A change counts from"
              value={every}
              onChange={(next) => { every = next; }}
              options={[{ value: 'from', label: 'This sprint on' }, { value: 'every', label: 'Every sprint' }]}
            />
          </div>
          <span class="text-[0.71875rem] text-fg-hint">
            {every === 'every' ? 'Finished sprints are counted again with the changed rule.' : 'Finished sprints keep the rule they were counted with.'}
          </span>
        </li>
        <li class="flex min-w-0 flex-col gap-2" data-testid="tracker-credit-which">
          <div class="flex flex-col">
            <span class="text-[0.8125rem] text-fg">Which tickets are support</span>
            <span class="text-[0.71875rem] text-fg-hint">A ticket of a type, with a label or on a board picked here is a support ticket while it has no estimate.</span>
          </div>
          {#each sites as s (s.site)}
            {@const picked = supportOf(s.site)}
            {@const read = typesOf(s)}
            {@const unknown = picked.types.filter((id) => !read.types.some((t) => t.id === id))}
            <div class="flex flex-col gap-1.5 pl-2" data-testid="tracker-credit-site" data-site={s.site}>
              {#if sites.length > 1}<span class="text-[0.75rem] text-fg-muted">{s.site}</span>{/if}
              <div class="flex flex-wrap items-center gap-1.5" data-testid="tracker-credit-types">
                <span class="w-14 text-[0.75rem] text-fg-muted">Types</span>
                {#each [...read.types, ...unknown.map((id) => ({ id, name: `Type ${id}` }))] as t (t.id)}
                  {@const chosen = picked.types.includes(t.id)}
                  <button
                    type="button"
                    class="{CHIP_BASE_CLASS} {chosen ? CHIP_SELECTED_CLASS : CHIP_RESTING_CLASS}"
                    aria-pressed={chosen}
                    disabled={locked}
                    onclick={() => editSupport(s.site, { list: 'types', id: t.id, on: !chosen })}
                    data-testid="tracker-credit-type"
                    data-type={t.id}
                  >{t.name}</button>
                {/each}
                {#if read.reading}
                  <span class="text-[0.71875rem] text-fg-hint">Reading the types…</span>
                {/if}
                {#each read.failed as f (f.key)}
                  <span class="flex flex-wrap items-center gap-1.5 text-[0.71875rem] text-error" data-testid="tracker-credit-types-failed">
                    {f.text}
                    <button type="button" class={GHOST_BUTTON_CLASS} onclick={f.retry} data-testid="tracker-credit-types-retry">Try again</button>
                  </span>
                {/each}
              </div>
              <div class="flex flex-wrap items-center gap-1.5" data-testid="tracker-credit-labels">
                <span class="w-14 text-[0.75rem] text-fg-muted">Labels</span>
                {#each picked.labels as label (label)}
                  <button
                    type="button"
                    class="{CHIP_BASE_CLASS} {CHIP_SELECTED_CLASS}"
                    title="Remove"
                    aria-label="Remove label {label}"
                    disabled={locked}
                    onclick={() => editSupport(s.site, { list: 'labels', id: label, on: false })}
                    data-testid="tracker-credit-label"
                  >{label} ×</button>
                {/each}
                <input
                  class="{INPUT_CLASS} !w-40"
                  aria-label="Add a support label"
                  placeholder="Add a label"
                  disabled={locked}
                  bind:value={labelDraft[s.site]}
                  onkeydown={(e) => { if (isLocalEnter(e)) { e.preventDefault(); addLabel(s.site); } }}
                  data-testid="tracker-credit-label-input"
                />
              </div>
              <div class="flex flex-wrap items-center gap-1.5" data-testid="tracker-credit-boards">
                <span class="w-14 text-[0.75rem] text-fg-muted">Boards</span>
                {#each s.boards as b (`${b.connection}/${b.board}`)}
                  {@const chosen = picked.boards.includes(b.board)}
                  <button
                    type="button"
                    class="{CHIP_BASE_CLASS} {chosen ? CHIP_SELECTED_CLASS : CHIP_RESTING_CLASS}"
                    aria-pressed={chosen}
                    disabled={locked}
                    onclick={() => editSupport(s.site, { list: 'boards', id: b.board, on: !chosen })}
                    data-testid="tracker-credit-board"
                    data-board={b.board}
                  >{b.name || `Board ${b.board}`}</button>
                {/each}
              </div>
            </div>
          {/each}
        </li>
      {/if}
    </ul>
  {/if}
</div>
