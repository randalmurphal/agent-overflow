<script lang="ts">
  // The popup behind the composer-toolbar "MCP" trigger. Lists every
  // server visible to the thread's provider + per-server status chip
  // + checkbox; the row's primary onSelect toggles the unified
  // Disabled flag (Claude: workspace-scoped; Codex: global). The
  // trailing action button surfaces the most useful next step for
  // the row's state (Sign in for needs-auth http/sse, Refresh
  // otherwise). A "Manage…" link in the footer navigates to the
  // Settings → MCP pane where CRUD lives.

  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import LogIn from 'lucide-svelte/icons/log-in';
  import Settings from 'lucide-svelte/icons/settings';
  import Icon from '../../primitives/Icon.svelte';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import {
    mcpServersStore,
    mcpStatusKey,
  } from '../../../stores/mcpServers.svelte';
  import type { MCPServer, MCPServerStatus } from '../../../stores/bindings';
  import { OpenExternalURL } from '../../../stores/bindings';
  import { OPEN_SETTINGS_EVENT } from '../../../stores/eventNames';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';

  interface Props {
    anchor: HTMLElement | undefined;
    open: boolean;
    pane: ThreadPane;
    onClose: () => void;
  }

  let { anchor, open, pane, onClose }: Props = $props();

  // Derive the fields the load effect actually depends on. The raw
  // `pane.thread` reference is replaced on every usage event / item
  // upsert / durable-status patch, so reading it inside the effect
  // would re-trigger the loader on every Codex streaming token while
  // the popup is mounted.
  let loadProvider = $derived(pane.thread?.provider ?? '');
  let loadThreadId = $derived(pane.threadId ?? '');
  let loadWorkspacePath = $derived(pane.thread?.workspacePath ?? '');
  let isPlaceholder = $derived(pane.hasDraftPlaceholder);

  $effect(() => {
    if (!(open && loadProvider)) return;
    const provider = loadProvider;
    const threadId = loadThreadId;
    const workspacePath = loadWorkspacePath;
    const placeholder = isPlaceholder;
    void (async () => {
      const [library] = await Promise.all([
        placeholder
          ? mcpServersStore.loadForNewThread(provider, workspacePath)
          : threadId
            ? mcpServersStore.loadForThread(threadId, provider)
            : Promise.resolve([]),
        mcpServersStore.loadStatuses(provider),
      ]);
      // Live sessions feed the cache continuously, so a thread that
      // has been active in this app session already has every row
      // populated. For inactive threads (or first open of the day)
      // the snapshot is missing entries — trigger an ephemeral
      // refresh so rows don't sit at "Not checked" until the user
      // clicks Refresh manually. refreshStatuses single-flights, so
      // concurrent opens collapse to one underlying CLI call.
      const snapshot = mcpServersStore.statuses;
      const missing = library.some(
        (s) => s.provider === provider && !snapshot.has(mcpStatusKey(s.provider, s.name)),
      );
      if (missing) {
        await mcpServersStore.refreshStatuses(provider).catch(() => undefined);
      }
    })();
  });

  type StatusKey = 'connected' | 'starting' | 'needs-auth' | 'failed' | 'unknown' | 'refreshing';

  function statusKey(status: MCPServerStatus | undefined, refreshing: boolean): StatusKey {
    if (refreshing && !status) return 'refreshing';
    if (!status) return 'unknown';
    const s = status.status as string;
    if (s === 'connected') return 'connected';
    if (s === 'starting') return 'starting';
    if (s === 'needs-auth') return 'needs-auth';
    if (s === 'failed') return 'failed';
    return 'unknown';
  }

  const STATUS_DOT: Record<StatusKey, string> = {
    connected: 'bg-success',
    starting: 'bg-accent/60 animate-pulse',
    'needs-auth': 'bg-warning',
    failed: 'bg-error',
    unknown: 'bg-fg-subtle/40',
    refreshing: 'bg-accent/60 animate-pulse',
  };

  const STATUS_LABEL: Record<StatusKey, string> = {
    connected: 'Connected',
    starting: 'Starting…',
    'needs-auth': 'Needs sign-in',
    failed: 'Failed',
    unknown: 'Not checked',
    refreshing: 'Checking…',
  };

  function describe(server: MCPServer, status: MCPServerStatus | undefined, key: StatusKey): string {
    const transport = server.transport ?? 'stdio';
    let detail = STATUS_LABEL[key];
    if (key === 'connected' && status && (status.toolCount ?? 0) > 0) {
      const n = status.toolCount ?? 0;
      detail = `Connected · ${n} tool${n === 1 ? '' : 's'}`;
    } else if (key === 'failed' && status?.error) {
      detail = `Failed · ${status.error.slice(0, 80)}`;
    }
    return `${transport} · ${detail}`;
  }

  async function toggleServer(server: MCPServer, enable: boolean): Promise<void> {
    try {
      const threadId = pane.threadId;
      if (pane.hasDraftPlaceholder) {
        await mcpServersStore.setDefaultEnabled(
          server.provider,
          pane.thread?.workspacePath ?? '',
          server.name,
          enable,
        );
        await mcpServersStore.loadForNewThread(server.provider, pane.thread?.workspacePath ?? '');
      } else if (threadId) {
        await mcpServersStore.setEnabled(threadId, server.name, enable);
        await mcpServersStore.loadForThread(threadId, pane.thread?.provider ?? '');
      }
    } catch (err) {
      addToast('error', `Failed to update MCP server: ${errString(err)}`);
    }
  }

  async function signIn(server: MCPServer): Promise<void> {
    if (server.disabled) {
      addToast('info', `Enable ${server.name} first, then sign in.`);
      return;
    }
    const threadId = pane.threadId;
    if (!threadId) {
      addToast('info', 'Start the thread before signing in.');
      return;
    }
    try {
      const res = await mcpServersStore.triggerAuth(threadId, server.name);
      if (res?.authUrl) {
        await OpenExternalURL(res.authUrl);
      } else {
        addToast('info', `Sign-in already complete for ${server.name}.`);
      }
    } catch (err) {
      addToast('error', `Sign-in failed for ${server.name}: ${errString(err)}`);
    }
  }

  async function refresh(server: MCPServer): Promise<void> {
    try {
      await mcpServersStore.fetchStatus(server.provider, server.name, true);
    } catch (err) {
      addToast('error', `Status check failed for ${server.name}: ${errString(err)}`);
    }
  }

  function openSettings(): void {
    onClose();
    window.dispatchEvent(
      new CustomEvent(OPEN_SETTINGS_EVENT, { detail: { section: 'mcp' } }),
    );
  }

  let provider = $derived(pane.thread?.provider ?? '');
  let visible = $derived(
    provider
      ? isPlaceholder
        ? mcpServersStore.serversForNewThread(provider, loadWorkspacePath)
        : loadThreadId
          ? mcpServersStore.serversForThread(loadThreadId, provider)
          : []
      : [],
  );
  let allStatuses = $derived(mcpServersStore.statuses);
  let refreshingProviders = $derived(mcpServersStore.refreshingProvider);
  let providerRefreshing = $derived(provider ? refreshingProviders.has(provider) : false);
</script>

<Popover
  {anchor}
  {open}
  {onClose}
  placement="top-start"
  role="none"
>
  <Menu ariaLabel="MCP servers" {onClose}>
    {#if visible.length === 0}
      <div class="px-3 py-4 text-[0.75rem] text-fg-muted">
        <div class="mb-2 font-medium text-fg">No MCP servers configured</div>
        <div class="text-fg-subtle">
          Add a server in Settings to expose extra tools to this thread.
        </div>
      </div>
      <MenuDivider />
      <MenuItem
        label="Manage MCP servers…"
        onSelect={openSettings}
      >
        {#snippet icon()}
          <Icon icon={Settings} size={13} strokeWidth={1.75} />
        {/snippet}
      </MenuItem>
    {:else}
      {#each visible as server (mcpStatusKey(server.provider, server.name))}
        {@const key0 = mcpStatusKey(server.provider, server.name)}
        {@const status = allStatuses.get(key0)}
        {@const key = statusKey(status, providerRefreshing)}
        {@const inSet = !server.disabled}
        {@const needsAuth = key === 'needs-auth' && (server.transport === 'http' || server.transport === 'sse' || server.transport === 'streamable_http')}
        <MenuItem
          label={server.name}
          description={describe(server, status, key)}
          onSelect={() => void toggleServer(server, !inSet)}
          actionLabel={needsAuth ? 'Sign in' : 'Refresh'}
          actionTitle={needsAuth ? `Sign in to ${server.name}` : `Re-check ${server.name}`}
          actionDisabled={providerRefreshing || (needsAuth && !inSet)}
          onAction={() => {
            if (needsAuth) void signIn(server);
            else void refresh(server);
          }}
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
          {#snippet action()}
            <Icon
              icon={needsAuth ? LogIn : RefreshCw}
              size={12}
              strokeWidth={1.75}
              class={providerRefreshing ? 'animate-spin' : ''}
            />
          {/snippet}
        </MenuItem>
      {/each}
      <MenuDivider />
      <MenuItem
        label="Manage MCP servers…"
        onSelect={openSettings}
      >
        {#snippet icon()}
          <Icon icon={Settings} size={13} strokeWidth={1.75} />
        {/snippet}
      </MenuItem>
    {/if}
  </Menu>
</Popover>
