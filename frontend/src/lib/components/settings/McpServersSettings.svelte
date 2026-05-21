<script lang="ts">
  // McpServersSettings: library management for MCP servers across
  // both providers. Owns the list view + add/edit toggle + the
  // binding RPCs that mutate each provider's native config file.
  // Per-row form lives in McpServerForm.
  //
  // Test-connection runs through the shared store's fetchStatus
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
  import { mcpServersStore, mcpStatusKey } from '../../stores/mcpServers.svelte';
  import type { MCPServer, MCPServerStatus } from '../../stores/bindings';
  import { MCPServer as MCPServerCtor } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';

  type ProviderKind = 'claude' | 'codex';
  type Transport = 'stdio' | 'http' | 'sse' | 'streamable_http';

  type EditTarget = { kind: 'add' } | { kind: 'edit'; provider: ProviderKind; name: string };

  let loading = $state(true);
  let saving = $state(false);
  let editing = $state<EditTarget | null>(null);
  let formError = $state<string | null>(null);
  let pendingDelete = $state<MCPServer | null>(null);

  function emptyInitial() {
    return {
      provider: 'claude' as ProviderKind,
      name: '',
      transport: 'stdio' as Transport,
      command: '',
      args: [] as string[],
      env: {} as Record<string, string>,
      url: '',
      headers: {} as Record<string, string>,
      bearerTokenEnv: '',
    };
  }

  let initialForForm = $state(emptyInitial());

  async function load(): Promise<void> {
    loading = true;
    try {
      await mcpServersStore.loadAllProviders();
      await Promise.all([
        mcpServersStore.loadStatuses('claude'),
        mcpServersStore.loadStatuses('codex'),
      ]);
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
      provider: (server.provider as ProviderKind) ?? 'claude',
      name: server.name,
      transport: (server.transport as Transport) ?? 'stdio',
      command: server.command ?? '',
      args: server.args ?? [],
      env: (server.env as Record<string, string>) ?? {},
      url: server.url ?? '',
      headers: (server.headers as Record<string, string>) ?? {},
      bearerTokenEnv: server.bearerTokenEnv ?? '',
    };
    editing = {
      kind: 'edit',
      provider: (server.provider as ProviderKind) ?? 'claude',
      name: server.name,
    };
    formError = null;
  }

  function cancelForm(): void {
    editing = null;
    formError = null;
  }

  function buildServerPayload(values: ReturnType<typeof emptyInitial>, existing?: MCPServer): MCPServer {
    return new MCPServerCtor({
      provider: values.provider,
      name: values.name,
      source: existing?.source,
      transport: values.transport,
      command: values.transport === 'stdio' ? values.command : undefined,
      args: values.transport === 'stdio' ? values.args : undefined,
      env: Object.keys(values.env).length > 0 ? values.env : undefined,
      url: values.transport !== 'stdio' ? values.url : undefined,
      headers:
        values.transport !== 'stdio' && Object.keys(values.headers).length > 0
          ? values.headers
          : undefined,
      bearerTokenEnv: values.transport !== 'stdio' ? values.bearerTokenEnv || undefined : undefined,
      disabled: existing?.disabled ?? false,
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
        await mcpServersStore.loadAllProviders();
        addToast('info', `Added MCP server "${values.name}"`);
      } else {
        const existing = mcpServersStore.servers.find(
          (s) => s.provider === target.provider && s.name === target.name,
        );
        if (!existing) {
          formError = 'Server no longer exists; refresh and try again.';
          return;
        }
        const payload = buildServerPayload(values, existing);
        await mcpServersStore.updateServer(payload);
        await mcpServersStore.loadAllProviders();
        addToast('info', `Saved MCP server "${values.name}"`);
      }
      editing = null;
    } catch (err) {
      formError = errString(err);
    } finally {
      saving = false;
    }
  }

  function requestDelete(server: MCPServer): void {
    pendingDelete = server;
  }

  async function confirmDelete(): Promise<void> {
    const server = pendingDelete;
    pendingDelete = null;
    if (!server) return;
    try {
      await mcpServersStore.deleteServer(server.provider, server.name);
      await mcpServersStore.loadAllProviders();
      addToast('info', `Deleted MCP server "${server.name}"`);
    } catch (err) {
      addToast('error', `Delete failed: ${errString(err)}`);
    }
  }

  async function handleTest(server: MCPServer): Promise<void> {
    try {
      await mcpServersStore.fetchStatus(server.provider, server.name, true);
    } catch (err) {
      addToast('error', `Status check failed for ${server.name}: ${errString(err)}`);
    }
  }

  function statusLabel(status: MCPServerStatus | undefined, refreshing: boolean): string {
    if (refreshing && !status) return 'Checking…';
    if (!status) return 'Not checked';
    const s = status.status as string;
    if (s === 'connected') {
      const n = status.toolCount ?? 0;
      return n > 0 ? `Connected · ${n} tool${n === 1 ? '' : 's'}` : 'Connected';
    }
    if (s === 'starting') return 'Starting…';
    if (s === 'needs-auth') return 'Needs sign-in';
    if (s === 'failed') return `Failed${status.error ? ` · ${status.error}` : ''}`;
    return 'Unknown';
  }

  function statusClass(status: MCPServerStatus | undefined, refreshing: boolean): string {
    if (refreshing && !status) return 'bg-accent/60 animate-pulse';
    if (!status) return 'bg-fg-subtle/40';
    const s = status.status as string;
    if (s === 'connected') return 'bg-success';
    if (s === 'starting') return 'bg-accent/60 animate-pulse';
    if (s === 'needs-auth') return 'bg-warning';
    if (s === 'failed') return 'bg-error';
    return 'bg-fg-subtle/40';
  }

  function isEditingRow(server: MCPServer): boolean {
    return (
      editing?.kind === 'edit' &&
      editing.provider === server.provider &&
      editing.name === server.name
    );
  }

  function isReadOnly(server: MCPServer): boolean {
    // Plugin/cloud Claude entries are read-only — backend refuses
    // Create/Update on them.
    return server.provider === 'claude' && !!server.source && server.source !== 'user';
  }

  let library = $derived(mcpServersStore.servers);
  let allStatuses = $derived(mcpServersStore.statuses);
  let refreshingProviders = $derived(mcpServersStore.refreshingProvider);
</script>

<section class="flex flex-col gap-5" data-testid="settings-mcp-section">
  <SettingsHeader
    eyebrow="Tools"
    title="MCP servers"
    description="Model Context Protocol servers from each provider's native config (Claude: ~/.claude.json; Codex: ~/.codex/config.toml). Each active server contributes tools the agent can call as mcp__<server>__<tool>. AO never stores secrets — use $&#123;VAR&#125; references for tokens and OAuth flows route through the provider."
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
      {#each library as server (mcpStatusKey(server.provider, server.name))}
        {@const key0 = mcpStatusKey(server.provider, server.name)}
        {@const refreshing = refreshingProviders.has(server.provider)}
        {@const status = allStatuses.get(key0)}
        {@const readOnly = isReadOnly(server)}
        <li class="rounded-[var(--radius-card)] border border-border-subtle bg-surface-1/40">
          <div class="flex items-start gap-3 px-4 py-3">
            <span
              class={['mt-1 inline-block h-[8px] w-[8px] shrink-0 rounded-full', statusClass(status, refreshing)].join(' ')}
              aria-hidden="true"
            ></span>
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2">
                <span class="text-[13px] font-medium text-fg">{server.name}</span>
                <span class="text-[11px] uppercase tracking-[0.12em] text-fg-subtle">{server.provider}</span>
                <span class="text-[11px] uppercase tracking-[0.12em] text-fg-subtle">·</span>
                <span class="text-[11px] uppercase tracking-[0.12em] text-fg-subtle">{server.transport}</span>
                {#if server.disabled}
                  <span class="rounded-full bg-warning/20 px-1.5 text-[10px] font-semibold text-warning">Disabled</span>
                {/if}
                {#if readOnly}
                  <span class="rounded-full bg-fg-subtle/15 px-1.5 text-[10px] font-semibold text-fg-subtle">
                    {server.source}
                  </span>
                {/if}
              </div>
              <p class="mt-0.5 text-[11.5px] text-fg-muted">
                {statusLabel(status, refreshing)}
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
                onclick={() => void handleTest(server)}
                class={GHOST_BUTTON_CLASS}
                disabled={refreshing}
                data-testid="mcp-status-{server.provider}-{server.name}"
              >
                <Icon icon={RefreshCw} size={12} strokeWidth={1.75} class={refreshing ? 'animate-spin' : ''} />
              </button>
              {#if !readOnly}
                <button
                  type="button"
                  title="Edit"
                  aria-label="Edit"
                  onclick={() => startEdit(server)}
                  class={GHOST_BUTTON_CLASS}
                  disabled={isEditingRow(server)}
                  data-testid="mcp-edit-{server.provider}-{server.name}"
                >
                  <Icon icon={Pencil} size={12} strokeWidth={1.75} />
                </button>
                <button
                  type="button"
                  title="Delete"
                  aria-label="Delete"
                  onclick={() => requestDelete(server)}
                  class={GHOST_BUTTON_CLASS}
                  data-testid="mcp-delete-{server.provider}-{server.name}"
                >
                  <Icon icon={Trash2} size={12} strokeWidth={1.75} />
                </button>
              {/if}
            </div>
          </div>
          {#if isEditingRow(server)}
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

<ConfirmDialog
  open={pendingDelete !== null}
  title="Delete MCP Server"
  description={pendingDelete
    ? `Delete MCP server "${pendingDelete.name}"? This removes it from ${pendingDelete.provider === 'claude' ? 'Claude' : 'Codex'} config.`
    : ''}
  confirmLabel="Delete"
  destructive={true}
  onConfirm={() => { void confirmDelete(); }}
  onCancel={() => { pendingDelete = null; }}
/>
