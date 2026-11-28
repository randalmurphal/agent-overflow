<script lang="ts">
  // The Forge accounts item of the Credit group (docs/specs/issues.md,
  // "Credit"): the GitHub and GitLab accounts whose approvals and comments
  // on pull requests the project's credit reads, one list for each tracker
  // connection of its Jira boards (trackerapp.ForgeAccountsView), with
  // the tracker person each counts for and how that is known. A person
  // says who an account is where the name match is wrong or missing, that
  // it is no person's (a bot, whose reviews then count for nobody), or
  // that it is whoever its name matches again; the list then shows what
  // the computer answered. The people to pick are the tracker's, searched
  // as typed (stores/trackerSetup.svelte.ts).
  import type { BoardPace, ForgeAccountInput, ForgeAccountView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { untrack } from 'svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { acquireForgeAccounts, forgeAccountKey, forgeAccountsOf, refreshForgeAccounts, sayForgeAccount, trackerPeople } from '../../stores/trackerSetup.svelte';
  import { boardProjects, boardWriteContext } from '../../stores/trackerTicketWrites.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import TicketPicker, { type PickItem, type PickList } from '../issues/TicketPicker.svelte';
  import { settingsComputer } from './settingsComputer';
  import { GHOST_BUTTON_CLASS } from './styles';

  interface Props {
    project: string;
    /** The project's Jira boards, from the page's one read. */
    boards: readonly BoardPace[];
    /** Why the page's editors are off; empty when they are on. */
    lockReason: string;
  }

  let { project, boards, lockReason }: Props = $props();

  const { backend } = settingsComputer();
  const compact = $derived(isCompactLayout());
  const locked = $derived(lockReason !== '');

  // Each tracker connection of the boards, with its site and boards, in
  // the boards' order.
  const connections = $derived.by(() => {
    const out: { connection: string; site: string; boards: BoardPace[] }[] = [];
    for (const b of boards) {
      const found = out.find((c) => c.connection === b.connection);
      if (found) found.boards.push(b);
      else out.push({ connection: b.connection, site: b.site ?? '', boards: [b] });
    }
    return out;
  });
  // What is held, as values: the page hands `project` as a read of its
  // whole setup, which changes with every save and must not take the
  // hold again.
  const holding = $derived(JSON.stringify([project, ...connections.map((c) => c.connection)]));

  $effect(() => {
    const [of, ...ids] = JSON.parse(holding) as string[];
    // The hold reads and writes the store's own state, which is not what
    // this effect follows.
    const releases = untrack(() => ids.map((id) => acquireForgeAccounts(of, backend, id)));
    return () => {
      for (const release of releases) release();
    };
  });

  /** The tracker projects of a connection's boards, which the Credit
   * group reads for its types: whose assignable people the search falls
   * back to. */
  function projectsOf(c: { boards: BoardPace[] }): string[] {
    const ids = new Set<string>();
    for (const b of c.boards) {
      for (const p of boardProjects(boardWriteContext({ backend, connection: b.connection, board: b.board }))?.value ?? []) ids.add(p.id);
    }
    return [...ids];
  }

  function personName(a: ForgeAccountView): string {
    return a.person?.name || a.person?.id || '';
  }

  /** Who the account counts for, and how that is known. */
  function whoText(a: ForgeAccountView): string {
    switch (a.how) {
      case 'paired':
        return `${personName(a)}, as set here`;
      case 'not_a_person':
        return 'Not a person, as set here. Its reviews count for nobody.';
      case 'own':
        return `${personName(a)}, the account this computer reads the forge as`;
      case 'name':
        return `${personName(a)}, matched by name`;
      case 'nobody':
        return a.name
          ? 'Nobody: no person in the tracker, or more than one, has this name. Its reviews count for nobody.'
          : 'Nobody: the forge gives this account no name. Its reviews count for nobody.';
      default:
        return 'Not matched yet. The tracker is asked within a few minutes.';
    }
  }

  const set = (a: ForgeAccountView): boolean => a.how === 'paired' || a.how === 'not_a_person';

  /** What the last word on an account failed with, by connection and
   * account, until its next word. */
  let failures = $state<Record<string, unknown>>({});
  /** The account whose people were last picked from, and the button its
   * list opened at; the list is open while `pickerOpen`. Kept after the
   * list closes, which returns focus to the button. */
  let picking = $state.raw<{ connection: string; account: ForgeAccountView; anchor: HTMLElement } | null>(null);
  let pickerOpen = $state(false);

  function rowKey(connection: string, a: { host: string; login: string }): string {
    return `${connection}\u0000${forgeAccountKey(a.host, a.login)}`;
  }

  async function say(connection: string, word: ForgeAccountInput): Promise<void> {
    const key = rowKey(connection, word);
    delete failures[key];
    try {
      await sayForgeAccount(backend, connection, word);
    } catch (err) {
      failures[key] = err;
    }
  }

  async function people(connection: string, projects: readonly string[], text: string): Promise<PickList> {
    const found = await trackerPeople(backend, connection, projects, text);
    return { items: found.users.map((u) => ({ id: u.id, name: u.name || u.id })), more: found.more ?? false };
  }

  function pick(item: PickItem | null): void {
    if (!picking || !item) return;
    const { connection, account } = picking;
    void say(connection, { host: account.host, login: account.login, who: 'person', person: item.id, person_name: item.name });
  }
</script>

<li class="flex min-w-0 flex-col gap-2" data-testid="tracker-forge-accounts">
  <div class="flex flex-col">
    <span class="text-[0.8125rem] text-fg">Forge accounts</span>
    <span class="text-[0.71875rem] text-fg-hint">
      An approval or comment on a pull request counts for the tracker person its GitHub or GitLab account is. An account is matched to the one person with its name; say who it is where that is wrong or missing.
    </span>
  </div>
  {#each connections as c (c.connection)}
    {@const held = forgeAccountsOf(backend, c.connection)}
    <div class="flex flex-col gap-1.5 pl-2" data-testid="tracker-forge-accounts-connection" data-connection={c.connection}>
      <div class="flex flex-wrap items-center gap-2">
        {#if connections.length > 1}<span class="text-[0.75rem] text-fg-muted">{c.site || c.boards.map((b) => b.name || `Board ${b.board}`).join(', ')}</span>{/if}
        <button
          type="button"
          class={GHOST_BUTTON_CLASS}
          disabled={held.reading}
          onclick={() => void refreshForgeAccounts(backend, c.connection)}
          data-testid="tracker-forge-accounts-refresh"
        >{held.reading ? 'Reading…' : 'Read again'}</button>
      </div>
      {#if held.error !== null}
        <p class="text-[0.71875rem] text-error" role="alert" data-testid="tracker-forge-accounts-error">
          The forge accounts could not be read: {userFacingError(held.error)}
        </p>
      {/if}
      {#if held.view}
        {#if held.view.accounts.length === 0}
          <p class="text-[0.71875rem] text-fg-hint" data-testid="tracker-forge-accounts-none">
            No account has approved or commented on a pull request of these boards' tickets that this computer has read.
          </p>
        {/if}
        <ul class="flex flex-col gap-1.5">
          {#each held.view.accounts as a (forgeAccountKey(a.host, a.login))}
            {@const key = rowKey(c.connection, a)}
            {@const saving = held.saving.has(forgeAccountKey(a.host, a.login))}
            <li class="flex min-w-0 flex-col gap-0.5" data-testid="tracker-forge-account" data-login={a.login} data-host={a.host} data-how={a.how}>
              <div class="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 {compact ? 'min-h-9' : ''}">
                <div class="flex min-w-0 flex-1 basis-56 flex-col">
                  <span class="text-[0.75rem] text-fg wrap-anywhere">{#if a.name && a.name !== a.login}{a.name} <span class="text-fg-muted">· {a.login} on {a.host}</span>{:else}{a.login} <span class="text-fg-muted">on {a.host}</span>{/if}</span>
                  <span class="text-[0.71875rem] {a.how === 'nobody' ? 'text-warning' : 'text-fg-hint'}" data-testid="tracker-forge-account-who">{whoText(a)}</span>
                </div>
                <div class="flex flex-wrap items-center gap-1.5">
                  <button
                    type="button"
                    class={GHOST_BUTTON_CLASS}
                    disabled={locked || saving}
                    title={lockReason || undefined}
                    aria-haspopup="dialog"
                    aria-expanded={pickerOpen && picking?.anchor.isConnected && picking.connection === c.connection && picking.account.host === a.host && picking.account.login === a.login}
                    onclick={(e) => { picking = { connection: c.connection, account: a, anchor: e.currentTarget }; pickerOpen = true; }}
                    data-testid="tracker-forge-account-pick"
                  >{a.person ? 'Change person…' : 'Pick a person…'}</button>
                  {#if a.how !== 'not_a_person'}
                    <button
                      type="button"
                      class={GHOST_BUTTON_CLASS}
                      disabled={locked || saving}
                      title={lockReason || 'A bot or a shared account: its reviews count for nobody'}
                      onclick={() => void say(c.connection, { host: a.host, login: a.login, who: 'not_a_person' })}
                      data-testid="tracker-forge-account-no-person"
                    >Not a person</button>
                  {/if}
                  {#if set(a)}
                    <button
                      type="button"
                      class={GHOST_BUTTON_CLASS}
                      disabled={locked || saving}
                      title={lockReason || 'Forget what was set here and match the account by its name again'}
                      onclick={() => void say(c.connection, { host: a.host, login: a.login, who: 'by_name' })}
                      data-testid="tracker-forge-account-by-name"
                    >Match by name</button>
                  {/if}
                </div>
              </div>
              {#if failures[key] !== undefined}
                <p class="text-[0.71875rem] text-error" role="alert" data-testid="tracker-forge-account-failed">{userFacingError(failures[key])}</p>
              {/if}
            </li>
          {/each}
        </ul>
        {#if held.view.more}
          <p class="text-[0.71875rem] text-fg-hint" data-testid="tracker-forge-accounts-more">
            More accounts have acted than are listed: those set here, then those matched to nobody, then the rest by name.
          </p>
        {/if}
      {:else if held.error === null}
        <p class="text-[0.71875rem] text-fg-hint">Reading the forge accounts…</p>
      {/if}
    </div>
  {/each}
</li>

{#if picking}
  {@const at = picking}
  {@const projects = projectsOf(connections.find((c) => c.connection === at.connection) ?? { boards: [] })}
  <TicketPicker
    anchor={at.anchor}
    open={pickerOpen}
    label="Who {at.account.login} is"
    load={(text) => people(at.connection, projects, text)}
    checked={new Set(at.account.person ? [at.account.person.id] : [])}
    onPick={pick}
    onClose={() => { pickerOpen = false; }}
  />
{/if}
