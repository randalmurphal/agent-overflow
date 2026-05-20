<script lang="ts">
  // McpServersSettings: library management for user-configured MCP
  // servers. Owns the list view + add/edit toggle + the binding RPCs
  // that mutate the library. Per-row form lives in McpServerForm.
  //
  // Test-connection runs through the shared store's probeServer
  // helper so the composer popup picks up cached results the moment
  // they land here (and vice versa).

  import { onMount } from 'svelte';
  import Plus from 'lucide-svelte/icons/plus';
  import Pencil from 'lucide-svelte/icons/pencil';
  import Trash2 from 'lucide-svelte/icons/trash-2';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import Icon from '../primitives/Icon.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import McpServerForm from './McpServerForm.svelte';
  import {
    PRIMARY_BUTTON_CLASS,
    GHOST_BUTTON_CLASS,
  } from './styles';
  import { mcpServersStore } from '../../stores/mcpServers.svelte';
  import type { MCPServer, MCPProbeResult } from '../../stores/bindings';
  import { MCPServer as MCPServerCtor } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';

  type Transport = 'stdio' | 'http' | 'sse';

  let loading = $state(true);
  let saving = $state(false);
  let editing = $state<{ kind: 'add' } | { kind: 'edit'; id: string } | null>(null);
  let formError = $state<string | null>(null);

  function emptyInitial() {
    return {
      name: '',
      transport: 'stdio' as Transport,
      command: '',
      args: [] as string[],
      env: {} as Record<string, string>,
      url: '',
      headers: {} as Record<string, string>,
      bearerEnv: '',
    };
  }

  let initialForForm = $state(emptyInitial());

  async function load(): Promise<void> {
    loading = true;
    try {
      await mcpServersStore.refreshLibrary();
      await mcpServersStore.refreshProbeSnapshot();
    } catch (err) {
      addToast('error', `Failed to load MCP servers: ${errString(err)}`);
    } finally {
      loading = false;
    }
  }

  onMount(() => {
    void load();
  });

  function startAdd(): void {
    initialForForm = emptyInitial();
    editing = { kind: 'add' };
    formError = null;
  }

  function startEdit(server: MCPServer): void {
    initialForForm = {
      name: server.name,
      transport: (server.transport as Transport) ?? 'stdio',
      command: server.command ?? '',
      args: server.args ?? [],
      env: (server.env as Record<string, string>) ?? {},
      url: server.url ?? '',
      headers: (server.headers as Record<string, string>) ?? {},
      bearerEnv: server.bearerEnv ?? '',
    };
    editing = { kind: 'edit', id: server.id };
    formError = null;
  }

  function cancelForm(): void {
    editing = null;
    formError = null;
  }

  function buildServerPayload(values: ReturnType<typeof emptyInitial>, existing?: MCPServer): MCPServer {
    return new MCPServerCtor({
      id: existing?.id ?? '',
      name: values.name,
      transport: values.transport,
      command: values.transport === 'stdio' ? values.command : undefined,
      args: values.transport === 'stdio' ? values.args : undefined,
      env: Object.keys(values.env).length > 0 ? values.env : undefined,
      url: values.transport !== 'stdio' ? values.url : undefined,
      headers:
        values.transport !== 'stdio' && Object.keys(values.headers).length > 0
          ? values.headers
          : undefined,
      bearerEnv: values.transport !== 'stdio' ? values.bearerEnv || undefined : undefined,
      enabled: existing?.enabled ?? true,
      createdAt: existing?.createdAt ?? 0,
      updatedAt: existing?.updatedAt ?? 0,
    });
  }

  async function handleSubmit(values: ReturnType<typeof emptyInitial>): Promise<void> {
    const target = editing;
    if (!target) return;
    saving = true;
    formError = null;
    try {
      if (target.kind === 'add') {
        const payload = buildServerPayload(values);
        await mcpServersStore.createServer(payload);
        addToast('info', `Added MCP server "${values.name}"`);
      } else {
        const existing = mcpServersStore.library.find((s) => s.id === target.id);
        if (!existing) {
          formError = 'Server no longer exists; refresh and try again.';
          return;
        }
        const payload = buildServerPayload(values, existing);
        await mcpServersStore.updateServer(payload);
        addToast('info', `Saved MCP server "${values.name}"`);
      }
      editing = null;
    } catch (err) {
      formError = errString(err);
    } finally {
      saving = false;
    }
  }

  async function handleDelete(server: MCPServer): Promise<void> {
    const confirmed = window.confirm(
      `Delete MCP server "${server.name}"? This removes it from every thread.`,
    );
    if (!confirmed) return;
    try {
      await mcpServersStore.deleteServer(server.id);
      addToast('info', `Deleted MCP server "${server.name}"`);
    } catch (err) {
      addToast('error', `Delete failed: ${errString(err)}`);
    }
  }

  async function handleProbe(server: MCPServer): Promise<void> {
    try {
      await mcpServersStore.probeServer(server.id, true);
    } catch (err) {
      addToast('error', `Probe failed for ${server.name}: ${errString(err)}`);
    }
  }

  function statusLabel(server: MCPServer, result: MCPProbeResult | undefined, probing: boolean): string {
    if (probing) return 'Checking…';
    if (!result) return 'Not checked';
    const status = result.status as string;
    if (status === 'ready') {
      return result.toolCount > 0
        ? `Ready · ${result.toolCount} tool${result.toolCount === 1 ? '' : 's'}`
        : 'Ready';
    }
    if (status === 'needs-auth') return 'Needs sign-in';
    if (status === 'failed') return `Failed${result.error ? ` · ${result.error}` : ''}`;
    return 'Unknown';
  }

  function statusClass(server: MCPServer, result: MCPProbeResult | undefined, probing: boolean): string {
    if (probing) return 'bg-accent/60 animate-pulse';
    if (!result) return 'bg-fg-subtle/40';
    const status = result.status as string;
    if (status === 'ready') return 'bg-success';
    if (status === 'needs-auth') return 'bg-warning';
    if (status === 'failed') return 'bg-error';
    return 'bg-fg-subtle/40';
  }

  let library = $derived(mcpServersStore.library);
  let probes = $derived(mcpServersStore.probeResults);
  let inFlight = $derived(mcpServersStore.probesInFlight);
</script>

<section class="flex flex-col gap-5" data-testid="settings-mcp-section">
  <SettingsHeader
    eyebrow="Tools"
    title="MCP servers"
    description="User-configured Model Context Protocol servers. Each enabled server contributes tools the agent can call as mcp__<server>__<tool>. AO never stores secrets — use $&#123;VAR&#125; references for tokens and OAuth flows route through the provider."
  />

  <div class="flex items-center gap-2">
    <button
      type="button"
      onclick={startAdd}
      class={PRIMARY_BUTTON_CLASS}
      disabled={editing?.kind === 'add'}
      data-testid="mcp-add-server"
    >
      <span class="inline-flex items-center gap-1.5">
        <Icon icon={Plus} size={12} strokeWidth={2} />
        Add server
      </span>
    </button>
    <button
      type="button"
      onclick={() => void load()}
      class={GHOST_BUTTON_CLASS}
      disabled={loading}
      data-testid="mcp-refresh-list"
    >
      Refresh
    </button>
  </div>

  {#if editing?.kind === 'add'}
    <McpServerForm
      mode="add"
      initial={initialForForm}
      initialError={formError}
      {saving}
      onSubmit={(values) => void handleSubmit(values)}
      onCancel={cancelForm}
    />
  {/if}

  {#if loading && library.length === 0}
    <p class="text-[12px] text-fg-muted">Loading…</p>
  {:else if library.length === 0}
    <p class="text-[12px] text-fg-muted">
      No MCP servers yet. Add one to expose extra tools to your threads.
    </p>
  {:else}
    <ul class="flex flex-col gap-2">
      {#each library as server (server.id)}
        {@const probing = inFlight.has(server.id)}
        {@const result = probes.get(server.id)}
        <li class="rounded-[var(--radius-card)] border border-border-subtle bg-surface-1/40">
          <div class="flex items-start gap-3 px-4 py-3">
            <span
              class={['mt-1 inline-block h-[8px] w-[8px] shrink-0 rounded-full', statusClass(server, result, probing)].join(' ')}
              aria-hidden="true"
            ></span>
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2">
                <span class="text-[13px] font-medium text-fg">{server.name}</span>
                <span class="text-[11px] uppercase tracking-[0.12em] text-fg-subtle">{server.transport}</span>
              </div>
              <p class="mt-0.5 text-[11.5px] text-fg-muted">
                {statusLabel(server, result, probing)}
              </p>
              {#if server.transport === 'stdio' && server.command}
                <p class="mt-1 truncate font-mono text-[11px] text-fg-subtle">
                  {server.command}{server.args && server.args.length > 0 ? ` ${server.args.join(' ')}` : ''}
                </p>
              {:else if server.url}
                <p class="mt-1 truncate font-mono text-[11px] text-fg-subtle">{server.url}</p>
              {/if}
            </div>
            <div class="flex shrink-0 items-center gap-1">
              <button
                type="button"
                title="Test connection"
                aria-label="Test connection"
                onclick={() => void handleProbe(server)}
                class={GHOST_BUTTON_CLASS}
                disabled={probing}
                data-testid="mcp-probe-{server.id}"
              >
                <Icon icon={RefreshCw} size={12} strokeWidth={1.75} class={probing ? 'animate-spin' : ''} />
              </button>
              <button
                type="button"
                title="Edit"
                aria-label="Edit"
                onclick={() => startEdit(server)}
                class={GHOST_BUTTON_CLASS}
                disabled={editing?.kind === 'edit' && editing.id === server.id}
                data-testid="mcp-edit-{server.id}"
              >
                <Icon icon={Pencil} size={12} strokeWidth={1.75} />
              </button>
              <button
                type="button"
                title="Delete"
                aria-label="Delete"
                onclick={() => void handleDelete(server)}
                class={GHOST_BUTTON_CLASS}
                data-testid="mcp-delete-{server.id}"
              >
                <Icon icon={Trash2} size={12} strokeWidth={1.75} />
              </button>
            </div>
          </div>
          {#if editing?.kind === 'edit' && editing.id === server.id}
            <div class="border-t border-border-subtle px-4 py-3">
              <McpServerForm
                mode="edit"
                initial={initialForForm}
                initialError={formError}
                {saving}
                onSubmit={(values) => void handleSubmit(values)}
                onCancel={cancelForm}
              />
            </div>
          {/if}
        </li>
      {/each}
    </ul>
  {/if}
</section>
