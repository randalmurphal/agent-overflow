<script lang="ts">
  // The popup behind the composer-toolbar "MCP" trigger. Rows come
  // from the provider-native listing for the pane's MCP entity (live
  // session truth when the thread has one, config + status cache
  // otherwise) and render instantly from the last load while a
  // background reload runs. The row's primary onSelect toggles the
  // server; the trailing action is a labeled button offering the
  // remedy that fits the row's state (utils/mcpRowAction owns the
  // precedence — Sign in / Sign in again / Reconnect / Refresh).
  //
  // The entity is HELD by McpServersTrigger, which outlives this popup;
  // opening only asks for a fresh listing and permits that listing to
  // chain a provider status fetch.

  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import { mcpRowAction } from '../../../utils/mcpRowAction';
  import {
    isMcpServersLoading,
    mcpRowKey,
    mcpRowsSourceThreadId,
    mcpTargetFor,
    peekMcpServers,
    peekMcpServersError,
    permitMcpStatusFetch,
    reconnectMcpServer,
    refreshMcpServers,
    refreshMcpServerStatus,
    setMcpServerEnabled,
    triggerMcpAuth,
  } from '../../../stores/mcpServers.svelte';
  import type { ThreadMCPServer } from '../../../stores/bindings';
  import { OpenExternalURL } from '../../../stores/bindings';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';

  interface Props {
    anchor: HTMLElement | undefined;
    open: boolean;
    pane: ThreadPane;
    // Forwarded to Popover/Menu verbatim so the close reason reaches the
    // trigger's focus-restore gating.
    onClose: (reason?: PopoverCloseReason) => void;
  }

  let { anchor, open, pane, onClose }: Props = $props();

  // Derive the fields the target actually depends on. The raw
  // `pane.thread` reference is replaced on every usage event / item
  // upsert / durable-status patch, so reading it inside the effect
  // would re-trigger the loader on every Codex streaming token while
  // the popup is mounted.
  let scopeProvider = $derived(pane.thread?.provider ?? '');
  let scopeThreadId = $derived(pane.threadId ?? '');
  let scopeWorkspacePath = $derived(pane.thread?.workspacePath ?? '');
  let target = $derived(mcpTargetFor(scopeProvider, scopeThreadId, scopeWorkspacePath));

  $effect(() => {
    const opened = target;
    if (!open || !opened) return;
    // Background refresh: cached rows stay on screen while the
    // authoritative listing loads, so opening the menu never blanks
    // or blocks on a provider round-trip. The permit is what lets that
    // listing chain a provider status fetch — released on close, so a
    // closed composer never spawns one.
    const release = permitMcpStatusFetch(opened.key);
    refreshMcpServers(opened.key);
    return release;
  });

  type StatusKey = 'connected' | 'starting' | 'needs-auth' | 'failed' | 'disabled' | 'unknown';

  function statusKey(row: ThreadMCPServer): StatusKey {
    const s = row.status;
    if (s === 'connected' || s === 'starting' || s === 'needs-auth' || s === 'failed' || s === 'disabled') {
      return s;
    }
    return 'unknown';
  }

  const STATUS_DOT: Record<StatusKey, string> = {
    connected: 'bg-success',
    starting: 'bg-accent/60 animate-pulse',
    'needs-auth': 'bg-warning',
    failed: 'bg-error',
    disabled: 'bg-fg-subtle/40',
    unknown: 'bg-fg-subtle/40',
  };

  const STATUS_LABEL: Record<StatusKey, string> = {
    connected: 'Connected',
    starting: 'Starting…',
    'needs-auth': 'Needs sign-in',
    failed: 'Failed',
    disabled: 'Disabled',
    unknown: 'Not checked',
  };

  function describe(row: ThreadMCPServer, key: StatusKey): string {
    if (key === 'connected' && (row.tools?.length ?? 0) > 0) {
      const n = row.tools?.length ?? 0;
      return `Connected · ${n} tool${n === 1 ? '' : 's'}`;
    }
    if (key === 'failed' && row.error) {
      return `Failed · ${row.error.slice(0, 80)}`;
    }
    return STATUS_LABEL[key];
  }

  async function toggleServer(row: ThreadMCPServer, enable: boolean): Promise<void> {
    if (!target) return;
    try {
      await setMcpServerEnabled(target, row.name, enable);
    } catch (err) {
      addToast('error', `Failed to update MCP server: ${errString(err)}`);
    }
  }

  async function signIn(row: ThreadMCPServer): Promise<void> {
    if (row.disabled) {
      addToast('info', `Enable ${row.name} first, then sign in.`);
      return;
    }
    if (!target) return;
    try {
      const res = await triggerMcpAuth(target, row.name);
      if (res?.authUrl) {
        await OpenExternalURL(res.authUrl);
      } else {
        addToast('info', `Sign-in already complete for ${row.name}.`);
      }
    } catch (err) {
      addToast('error', `Sign-in failed for ${row.name}: ${errString(err)}`);
    }
  }

  async function reconnect(row: ThreadMCPServer): Promise<void> {
    if (!target?.threadId) return;
    try {
      await reconnectMcpServer(target, row.name);
    } catch (err) {
      addToast('error', `Reconnect failed for ${row.name}: ${errString(err)}`);
    }
  }

  async function refresh(row: ThreadMCPServer): Promise<void> {
    try {
      await refreshMcpServerStatus(row.provider, row.name);
    } catch (err) {
      addToast('error', `Status check failed for ${row.name}: ${errString(err)}`);
    }
  }

  let rows = $derived(peekMcpServers(target?.key ?? null));
  let loading = $derived(isMcpServersLoading(target?.key ?? null));
  // Reconnect runs against a LIVE session, and one workspace key is shared by
  // every pane on it — so `row.source === 'session'` only says some pane's
  // session answered. Offering the action to a pane whose own thread is not
  // that one renders a button whose only outcome is a backend refusal toast.
  let rowsFromOwnSession = $derived(
    !!target?.threadId && mcpRowsSourceThreadId(target.key) === target.threadId,
  );
  // The listing's own failure is state, not a toast: it persists while the
  // store's retry curve runs, and a toast per retry would be noise.
  let listingError = $derived(peekMcpServersError(target?.key ?? null));
</script>

<Popover
  {anchor}
  {open}
  {onClose}
  placement="top-start"
  role="none"
>
  <Menu ariaLabel="MCP servers" {onClose}>
    {#if listingError}
      <div class="px-3 py-2 text-[0.75rem] text-error" data-testid="mcp-menu-error">
        MCP server listing failed: {listingError}
      </div>
    {/if}
    {#if rows.length === 0}
      <div class="px-3 py-4 text-[0.75rem] text-fg-muted" data-testid="mcp-menu-empty">
        {#if loading}
          <div class="font-medium text-fg">Loading MCP servers…</div>
        {:else if !listingError}
          <div class="mb-2 font-medium text-fg">No MCP servers configured</div>
          <div class="text-fg-subtle">
            Servers configured for {scopeProvider === 'codex' ? 'Codex' : 'Claude Code'} appear
            here with a per-thread toggle.
          </div>
        {/if}
      </div>
    {:else}
      {#each rows as row (mcpRowKey(row.provider, row.name))}
        {@const key = statusKey(row)}
        {@const inSet = !row.disabled}
        {@const canReconnect = row.source === 'session' && rowsFromOwnSession}
        {@const act = mcpRowAction(row, canReconnect)}
        <MenuItem
          label={row.name}
          description={describe(row, key)}
          onSelect={() => void toggleServer(row, !inSet)}
          actionText={inSet ? act.label : undefined}
          actionLabel={act.title}
          actionTitle={act.title}
          actionDisabled={loading}
          onAction={inSet
            ? () => {
                if (act.kind === 'sign-in') void signIn(row);
                else if (act.kind === 'reconnect') void reconnect(row);
                else void refresh(row);
              }
            : undefined}
        >
          {#snippet indicator()}
            <span
              class="relative inline-flex h-4 w-7 shrink-0 items-center rounded-full border transition-all duration-200
                {inSet ? 'border-accent/40 bg-accent/85' : 'border-border bg-surface-2/80'}"
              aria-hidden="true"
            >
              <span
                class="block h-3 w-3 rounded-full bg-text-primary shadow-sheet transition-transform duration-200
                  {inSet ? 'translate-x-[13px]' : 'translate-x-[1px]'}"
              ></span>
            </span>
          {/snippet}
          {#snippet icon()}
            <span
              class={['inline-block h-[8px] w-[8px] rounded-full', STATUS_DOT[key]].join(' ')}
              aria-hidden="true"
              data-mcp-status={key}
            ></span>
          {/snippet}
        </MenuItem>
      {/each}
    {/if}
  </Menu>
</Popover>
