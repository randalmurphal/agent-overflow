<script lang="ts">
  // The popup behind the composer-toolbar "MCP" trigger. Rows come
  // from the provider-native listing for the pane's scope (live
  // session truth when the thread has one, config + status cache
  // otherwise) and render instantly from the last load while a
  // background reload runs. The row's primary onSelect toggles the
  // server; the trailing action is the most useful next step for the
  // row's state: Sign in for needs-auth, Reconnect for live-session
  // rows, Refresh (ephemeral status re-check) for config rows.

  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import LogIn from 'lucide-svelte/icons/log-in';
  import Icon from '../../primitives/Icon.svelte';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import {
    mcpServersStore,
    mcpRowKey,
    mcpScopeFor,
  } from '../../../stores/mcpServers.svelte';
  import type { ThreadMCPServer } from '../../../stores/bindings';
  import { OpenExternalURL } from '../../../stores/bindings';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';

  interface Props {
    anchor: HTMLElement | undefined;
    open: boolean;
    pane: ThreadPane;
    onClose: () => void;
  }

  let { anchor, open, pane, onClose }: Props = $props();

  // Derive the fields the scope actually depends on. The raw
  // `pane.thread` reference is replaced on every usage event / item
  // upsert / durable-status patch, so reading it inside the effect
  // would re-trigger the loader on every Codex streaming token while
  // the popup is mounted.
  let scopeProvider = $derived(pane.thread?.provider ?? '');
  let scopeThreadId = $derived(pane.threadId ?? '');
  let scopeWorkspacePath = $derived(pane.thread?.workspacePath ?? '');
  let isPlaceholder = $derived(pane.hasDraftPlaceholder);

  let scope = $derived(
    mcpScopeFor(scopeProvider, scopeThreadId, scopeWorkspacePath, isPlaceholder),
  );

  $effect(() => {
    if (!(open && scope)) return;
    // Background refresh: cached rows stay on screen while the
    // authoritative listing loads, so opening the menu never blanks
    // or blocks on a provider round-trip.
    void mcpServersStore.load(scope).catch((err: unknown) => {
      addToast('error', `MCP server listing failed: ${errString(err)}`);
    });
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
    if (!scope) return;
    try {
      await mcpServersStore.setEnabled(scope, row.name, enable);
    } catch (err) {
      addToast('error', `Failed to update MCP server: ${errString(err)}`);
    }
  }

  async function signIn(row: ThreadMCPServer): Promise<void> {
    if (row.disabled) {
      addToast('info', `Enable ${row.name} first, then sign in.`);
      return;
    }
    if (scope?.kind !== 'thread') {
      addToast('info', 'Start the thread before signing in.');
      return;
    }
    try {
      const res = await mcpServersStore.triggerAuth(scope.threadId, row.name);
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
    if (scope?.kind !== 'thread') return;
    try {
      await mcpServersStore.reconnect(scope, row.name);
    } catch (err) {
      addToast('error', `Reconnect failed for ${row.name}: ${errString(err)}`);
    }
  }

  async function refresh(row: ThreadMCPServer): Promise<void> {
    try {
      await mcpServersStore.refreshStatus(row.provider, row.name);
    } catch (err) {
      addToast('error', `Status check failed for ${row.name}: ${errString(err)}`);
    }
  }

  let rows = $derived(scope ? mcpServersStore.rowsFor(scope) : []);
  let loading = $derived(scope ? mcpServersStore.isLoading(scope) : false);
</script>

<Popover
  {anchor}
  {open}
  {onClose}
  placement="top-start"
  role="none"
>
  <Menu ariaLabel="MCP servers" {onClose}>
    {#if rows.length === 0}
      <div class="px-3 py-4 text-[0.75rem] text-fg-muted" data-testid="mcp-menu-empty">
        {#if loading}
          <div class="font-medium text-fg">Loading MCP servers…</div>
        {:else}
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
        {@const needsAuth = key === 'needs-auth'}
        {@const canReconnect = row.source === 'session' && scope?.kind === 'thread'}
        {#snippet rowAction()}
          <Icon
            icon={needsAuth ? LogIn : RefreshCw}
            size={12}
            strokeWidth={1.75}
            class={loading ? 'animate-spin' : ''}
          />
        {/snippet}
        <MenuItem
          label={row.name}
          description={describe(row, key)}
          onSelect={() => void toggleServer(row, !inSet)}
          action={inSet ? rowAction : undefined}
          actionLabel={needsAuth ? 'Sign in' : canReconnect ? 'Reconnect' : 'Refresh'}
          actionTitle={needsAuth
            ? `Sign in to ${row.name}`
            : canReconnect
              ? `Reconnect ${row.name}`
              : `Re-check ${row.name}`}
          actionDisabled={loading}
          onAction={inSet
            ? () => {
                if (needsAuth) void signIn(row);
                else if (canReconnect) void reconnect(row);
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
                class="block h-3 w-3 rounded-full bg-text-primary shadow-sm transition-transform duration-200
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
