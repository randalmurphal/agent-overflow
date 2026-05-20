<script lang="ts">
  // The popup behind the composer-toolbar "MCP" trigger. Lists every
  // library row + per-server status chip + checkbox; the row's
  // primary onSelect toggles whether that server is enabled for the
  // current thread. The trailing action button surfaces the most
  // useful next step for the row's state (Sign in for needs-auth
  // http/sse, Refresh otherwise). A "Manage…" link in the footer
  // navigates to the Settings → MCP pane where CRUD lives.

  import { onMount } from 'svelte';
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
  } from '../../../stores/mcpServers.svelte';
  import type { MCPServer, MCPProbeResult } from '../../../stores/bindings';
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

  onMount(() => {
    // Idempotent — the store no-ops on repeated calls.
    void mcpServersStore.ensureInitialized();
  });

  $effect(() => {
    if (open && pane.threadId) {
      void mcpServersStore.loadThreadServers(pane.threadId);
    }
  });

  type StatusKey = 'ready' | 'needs-auth' | 'failed' | 'unknown' | 'probing';

  function statusKey(server: MCPServer, result: MCPProbeResult | undefined, probing: boolean): StatusKey {
    if (probing) return 'probing';
    if (!result) return 'unknown';
    const status = result.status as string;
    if (status === 'ready') return 'ready';
    if (status === 'needs-auth') return 'needs-auth';
    if (status === 'failed') return 'failed';
    return 'unknown';
  }

  const STATUS_DOT: Record<StatusKey, string> = {
    ready: 'bg-success',
    'needs-auth': 'bg-warning',
    failed: 'bg-error',
    unknown: 'bg-fg-subtle/40',
    probing: 'bg-accent/60 animate-pulse',
  };

  const STATUS_LABEL: Record<StatusKey, string> = {
    ready: 'Ready',
    'needs-auth': 'Needs sign-in',
    failed: 'Failed',
    unknown: 'Not checked',
    probing: 'Checking…',
  };

  function describe(server: MCPServer, result: MCPProbeResult | undefined, key: StatusKey): string {
    const transport = server.transport ?? 'stdio';
    let detail = STATUS_LABEL[key];
    if (key === 'ready' && result && result.toolCount > 0) {
      detail = `Ready · ${result.toolCount} tool${result.toolCount === 1 ? '' : 's'}`;
    } else if (key === 'failed' && result?.error) {
      detail = `Failed · ${result.error.slice(0, 80)}`;
    }
    return `${transport} · ${detail}`;
  }

  async function toggleServer(server: MCPServer, enabled: boolean): Promise<void> {
    if (!pane.threadId) return;
    const current = mcpServersStore.threadServers(pane.threadId);
    let next: string[];
    if (enabled) {
      if (current.includes(server.id)) return;
      next = [...current, server.id];
    } else {
      if (!current.includes(server.id)) return;
      next = current.filter((id) => id !== server.id);
    }
    try {
      await mcpServersStore.setThreadServers(pane.threadId, next);
    } catch (err) {
      addToast('error', `Failed to update MCP servers: ${errString(err)}`);
    }
  }

  async function signIn(server: MCPServer): Promise<void> {
    if (!pane.threadId) return;
    const enabled = mcpServersStore.threadServers(pane.threadId).includes(server.id);
    if (!enabled) {
      // The backend enforces the same rule; surfacing it here avoids a
      // round-trip and keeps the affordance disabled in the obvious case.
      addToast('info', `Enable ${server.name} first, then sign in.`);
      return;
    }
    try {
      const res = await mcpServersStore.triggerAuth(pane.threadId, server.id);
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
      await mcpServersStore.probeServer(server.id, true);
    } catch (err) {
      addToast('error', `Probe failed for ${server.name}: ${errString(err)}`);
    }
  }

  function openSettings(): void {
    onClose();
    window.dispatchEvent(
      new CustomEvent(OPEN_SETTINGS_EVENT, { detail: { section: 'mcp' } }),
    );
  }

  let library = $derived(mcpServersStore.library);
  let probes = $derived(mcpServersStore.probeResults);
  let inFlight = $derived(mcpServersStore.probesInFlight);
  let threadId = $derived(pane.threadId ?? '');
  let enabledSet = $derived(new Set(threadId ? mcpServersStore.threadServers(threadId) : []));
</script>

<Popover
  {anchor}
  {open}
  {onClose}
  placement="top-start"
  role="none"
>
  <Menu ariaLabel="MCP servers" {onClose}>
    {#if library.length === 0}
      <div class="px-3 py-4 text-[12px] text-fg-muted">
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
      {#each library as server (server.id)}
        {@const probing = inFlight.has(server.id)}
        {@const result = probes.get(server.id)}
        {@const key = statusKey(server, result, probing)}
        {@const inSet = enabledSet.has(server.id)}
        {@const needsAuth = key === 'needs-auth' && (server.transport === 'http' || server.transport === 'sse')}
        <MenuItem
          label={server.name}
          description={describe(server, result, key)}
          checked={inSet}
          onSelect={() => void toggleServer(server, !inSet)}
          actionLabel={needsAuth ? 'Sign in' : 'Refresh'}
          actionTitle={needsAuth ? `Sign in to ${server.name}` : `Re-check ${server.name}`}
          actionDisabled={probing || (needsAuth && !inSet)}
          onAction={() => {
            if (needsAuth) void signIn(server);
            else void refresh(server);
          }}
        >
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
              class={probing ? 'animate-spin' : ''}
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
