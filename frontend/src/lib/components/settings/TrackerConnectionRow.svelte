<script lang="ts">
  // One tracker connection: who it is signed in as or what is wrong, its
  // token's expiry, its switch, Check, and the rest in a menu. A Jira row
  // shows its Tempo connection under it as a row of its own. Every action
  // goes through the connections store, which settles once the list shows
  // the result; a failure stays under the row until its next action.
  import MoreHorizontal from '@lucide/svelte/icons/ellipsis';
  import type { TrackerConnection } from '../../stores/bindings';
  import type { BackendKey } from '../../transport/backendKey';
  import {
    addTrackerConnection,
    editTrackerKeyFile,
    openTrackerTokenPage,
    removeTrackerConnection,
    renameTrackerConnection,
    setTrackerConnectionEnabled,
    setTrackerConnectionTokenExpiry,
    verifyTrackerConnection,
  } from '../../stores/trackerConnections.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { relativeTime } from '../../utils/format';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import TrackerConnectionRow from './TrackerConnectionRow.svelte';
  import TrackerConnectionSetup from './TrackerConnectionSetup.svelte';
  import TrackerTokenExpiry from './TrackerTokenExpiry.svelte';
  import { GHOST_BUTTON_CLASS, INPUT_CLASS, SECONDARY_BUTTON_CLASS } from './styles';
  import {
    TONE_CLASS,
    tokenExpiry,
    trackerConnectionName,
    trackerConnectionStatus,
    trackerKindName,
  } from './trackerConnectionStatus';

  let {
    backend,
    connection,
    tempo,
    host,
    operateReason,
  }: {
    backend: BackendKey;
    connection: TrackerConnection;
    /** The Tempo connection that belongs to this Jira one. */
    tempo?: TrackerConnection;
    host: boolean;
    /** Why this page cannot change connections; '' when it can. */
    operateReason: string;
  } = $props();

  let name = $derived(trackerConnectionName(connection));
  let status = $derived(trackerConnectionStatus(connection));
  // Read with the row: a new answer (every read makes new rows) or a
  // return to the window moves it.
  let expiry = $derived(tokenExpiry(connection.tokenExpiresAt, Date.now()));
  let where = $derived(
    connection.kind === 'jira'
      ? `${connection.site.replace(/^https:\/\//, '')} · ${connection.account}`
      : connection.kind === 'linear'
        ? `linear.app/${connection.site}`
        : '',
  );
  let locked = $derived(operateReason !== '');

  let busy = $state<string | null>(null);
  let error = $state<unknown>(null);
  let renaming = $state(false);
  let renameValue = $state('');
  let editingExpiry = $state(false);
  let renewing = $state(false);
  let confirmingRemove = $state(false);

  async function run(what: string, action: () => Promise<unknown>): Promise<boolean> {
    busy = what;
    error = null;
    try {
      await action();
      return true;
    } catch (err) {
      error = err;
      return false;
    } finally {
      busy = null;
    }
  }

  const check = (): Promise<boolean> => run('check', () => verifyTrackerConnection(backend, connection.id));
  const openKeyFile = (): Promise<boolean> => run('key-file', () => editTrackerKeyFile(backend, connection.id));
  const openTokenPage = (): Promise<boolean> => run('token-page', () => openTrackerTokenPage(backend, connection.id));
  const saveExpiry = (expiresAt: number): Promise<boolean> =>
    run('expiry', () => setTrackerConnectionTokenExpiry(backend, connection.id, expiresAt));

  async function rename(): Promise<void> {
    if (await run('rename', () => renameTrackerConnection(backend, connection.id, renameValue))) renaming = false;
  }

  function addTempo(): Promise<boolean> {
    return run('tempo', () => addTrackerConnection(backend, { kind: 'tempo', site: '', parentId: connection.id }));
  }

  function remove(): void {
    confirmingRemove = false;
    void run('remove', () => removeTrackerConnection(backend, connection.id));
  }

  let removeDescription = $derived(
    connection.kind === 'tempo'
      ? `This computer deletes the Tempo key file and stops logging time through it. To use Tempo again, add it and save a new token.`
      : `This computer deletes its key file${tempo ? ', its Tempo time logging' : ''} and every board a project lists from it, and keeps no copy of its issues. To use it again, add it and save a new token.`,
  );

  let trigger: HTMLButtonElement | undefined = $state(undefined);
  let menuOpen = $state(false);

  function closeMenu(reason?: PopoverCloseReason): void {
    menuOpen = false;
    restorePickerFocus(reason, { triggerEl: trigger });
  }

  // A menu row starts its action with focus back on the trigger.
  function pick(action: () => void): void {
    trigger?.focus();
    menuOpen = false;
    action();
  }
</script>

<li class="flex flex-col gap-2" data-testid="tracker-connection-{connection.id}" data-kind={connection.kind}>
  <div class="flex items-start gap-3">
    <div class="flex min-w-0 flex-1 flex-col gap-0.5">
      <div class="flex min-w-0 flex-wrap items-baseline gap-x-2">
        <span class="truncate text-[0.8125rem] font-medium text-fg" data-testid="tracker-connection-name">{name}</span>
        <span class="text-[0.6875rem] text-fg-hint">{trackerKindName(connection.kind)}</span>
      </div>
      {#if where}
        <span class="truncate text-[0.71875rem] text-fg-hint">{where}</span>
      {/if}
      <span class="text-[0.75rem] {TONE_CLASS[status.tone]}" data-testid="tracker-connection-status" data-tone={status.tone}>
        {status.label}{#if status.tone === 'ok' && connection.verifiedAt > 0}<span class="text-fg-hint">{` · checked ${relativeTime(connection.verifiedAt, 'locale', backend)}`}</span>{/if}
      </span>
      {#if status.detail}
        <span class="text-[0.71875rem] text-fg-muted" data-testid="tracker-connection-detail">{status.detail}</span>
      {/if}
      {#if expiry}
        <span class="text-[0.71875rem] {TONE_CLASS[expiry.tone]}" data-testid="tracker-connection-expiry" data-tone={expiry.tone}>{expiry.text}</span>
      {/if}
    </div>
    <ToggleSwitch
      checked={connection.enabled}
      disabled={locked || busy !== null}
      title={operateReason || undefined}
      ariaLabel={connection.enabled ? `Turn off ${name}` : `Turn on ${name}`}
      onToggle={(on) => void run('enabled', () => setTrackerConnectionEnabled(backend, connection.id, on))}
    />
  </div>

  <div class="flex flex-wrap items-center gap-2">
    <button
      type="button"
      class={SECONDARY_BUTTON_CLASS}
      disabled={locked || busy !== null}
      title={operateReason || undefined}
      onclick={check}
      data-testid="tracker-connection-check"
    >{busy === 'check' ? 'Checking…' : 'Check'}</button>
    {#if expiry?.renew && !renewing}
      <button type="button" class={SECONDARY_BUTTON_CLASS} onclick={() => (renewing = true)} data-testid="tracker-connection-renew">Renew</button>
    {/if}
    <button
      bind:this={trigger}
      type="button"
      class="{GHOST_BUTTON_CLASS} inline-flex items-center compact:select-none"
      aria-haspopup="menu"
      aria-expanded={menuOpen}
      aria-label="More for {name}"
      onclick={() => (menuOpen = !menuOpen)}
      data-testid="tracker-connection-menu"
    >
      <Icon icon={MoreHorizontal} size={14} strokeWidth={2} />
    </button>
  </div>

  {#if renaming}
    <form
      class="flex flex-wrap items-center gap-2"
      onsubmit={(e) => {
        e.preventDefault();
        void rename();
      }}
    >
      <input
        class="{INPUT_CLASS} max-w-[18rem]"
        aria-label="Name for {name}"
        bind:value={renameValue}
        maxlength={200}
        data-testid="tracker-connection-rename-input"
      />
      <button type="submit" class={SECONDARY_BUTTON_CLASS} disabled={busy !== null}>Save name</button>
      <button type="button" class={GHOST_BUTTON_CLASS} onclick={() => (renaming = false)}>Cancel</button>
    </form>
  {/if}

  {#if editingExpiry}
    <TrackerTokenExpiry
      expiresAt={connection.tokenExpiresAt}
      disabled={locked || busy !== null}
      disabledReason={operateReason || undefined}
      onSave={saveExpiry}
      onCancel={() => (editingExpiry = false)}
    />
  {/if}

  {#if (status.needsSetup || renewing) && connection.enabled}
    <TrackerConnectionSetup
      {connection}
      {host}
      busy={busy !== null}
      {operateReason}
      onOpenTokenPage={openTokenPage}
      onOpenKeyFile={openKeyFile}
      onSaveExpiry={saveExpiry}
      onCheck={async () => {
        if ((await check()) && renewing) renewing = false;
      }}
    />
    {#if renewing}
      <div>
        <button type="button" class={GHOST_BUTTON_CLASS} onclick={() => (renewing = false)}>Done</button>
      </div>
    {/if}
  {/if}

  {#if error !== null}
    <p class="text-[0.71875rem] text-error" role="alert" data-testid="tracker-connection-error">{userFacingError(error)}</p>
  {/if}

  {#if tempo}
    <ul class="ml-4 border-l border-border-subtle pl-3">
      <TrackerConnectionRow {backend} connection={tempo} {host} {operateReason} />
    </ul>
  {/if}
  <Popover anchor={trigger} open={menuOpen} onClose={closeMenu} placement="bottom-end" role="none" claimTab restoreFocusTo={trigger}>
    <Menu ariaLabel="More for {name}" minWidthClass="w-60 max-w-[calc(100vw-1rem)] min-w-0" onClose={closeMenu}>
      <MenuItem
        label="Rename"
        disabled={locked}
        title={operateReason || undefined}
        testId="tracker-connection-menu-rename"
        onSelect={() =>
          pick(() => {
            renameValue = connection.label;
            renaming = true;
          })}
      />
      <MenuItem
        label={connection.tokenExpiresAt > 0 ? 'Change token expiry' : 'Set token expiry'}
        disabled={locked}
        title={operateReason || undefined}
        testId="tracker-connection-menu-expiry"
        onSelect={() => pick(() => (editingExpiry = true))}
      />
      {#if host}
        <MenuItem label="Open the key file" testId="tracker-connection-menu-key-file" onSelect={() => pick(() => void openKeyFile())} />
        <MenuItem
          label={connection.kind === 'tempo' ? 'Open Jira' : 'Open the token page'}
          testId="tracker-connection-menu-token-page"
          onSelect={() => pick(() => void openTokenPage())}
        />
      {/if}
      {#if connection.kind === 'jira' && !tempo}
        <MenuItem
          label="Add Tempo time logging"
          description="Log time to Tempo with this Jira account."
          wrap
          disabled={locked}
          title={operateReason || undefined}
          testId="tracker-connection-menu-add-tempo"
          onSelect={() => pick(() => void addTempo())}
        />
      {/if}
      <MenuItem
        label="Remove"
        variant="danger"
        disabled={locked}
        title={operateReason || undefined}
        testId="tracker-connection-menu-remove"
        onSelect={() => pick(() => (confirmingRemove = true))}
      />
    </Menu>
  </Popover>

  <ConfirmDialog
    open={confirmingRemove}
    title="Remove {name}?"
    description={removeDescription}
    confirmLabel="Remove"
    destructive
    onConfirm={remove}
    onCancel={() => (confirmingRemove = false)}
  />
</li>
