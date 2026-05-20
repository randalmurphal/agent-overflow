<script lang="ts">
  // McpServerForm: the add/edit form for an MCP server. Owns input
  // fields + client-side validation + the transport-conditional
  // sections. The parent decides "Add" vs "Save", drives persistence,
  // and stitches the result into the surrounding list.
  //
  // No secret storage: env values and request headers may contain
  // `${VAR}` references that the provider expands at spawn time. The
  // helper text under each section makes the env-var indirection
  // explicit so users don't drop raw tokens into the form expecting
  // them to be stored securely.

  import {
    INPUT_CLASS,
    PRIMARY_BUTTON_CLASS,
    SECONDARY_BUTTON_CLASS,
    SELECT_CLASS,
  } from './styles';

  type ProviderKind = 'claude' | 'codex';
  type Transport = 'stdio' | 'http' | 'sse' | 'streamable_http';

  interface InitialValues {
    provider: ProviderKind;
    name: string;
    transport: Transport;
    command: string;
    args: string[];
    env: Record<string, string>;
    url: string;
    headers: Record<string, string>;
    bearerTokenEnv: string;
  }

  interface SubmitValues extends InitialValues {}

  interface Props {
    mode: 'add' | 'edit';
    initial: InitialValues;
    initialError?: string | null;
    saving: boolean;
    onSubmit: (values: SubmitValues) => void;
    onCancel: () => void;
  }

  const { mode, initial, initialError = null, saving, onSubmit, onCancel }: Props =
    $props();

  let provider = $state<ProviderKind>('claude');
  let name = $state('');
  let transport = $state<Transport>('stdio');
  let command = $state('');
  let argsText = $state('');
  let envText = $state('');
  let url = $state('');
  let headersText = $state('');
  let bearerTokenEnv = $state('');
  let formError = $state<string | null>(null);

  // Re-sync local state when initial props change (parent re-uses the
  // form across edit targets). Each $effect runs only when the
  // corresponding prop changes.
  $effect(() => {
    provider = initial.provider;
  });
  $effect(() => {
    name = initial.name;
  });
  $effect(() => {
    transport = initial.transport;
  });
  $effect(() => {
    command = initial.command;
  });
  $effect(() => {
    argsText = initial.args.join(' ');
  });
  $effect(() => {
    envText = recordToLines(initial.env);
  });
  $effect(() => {
    url = initial.url;
  });
  $effect(() => {
    headersText = recordToLines(initial.headers);
  });
  $effect(() => {
    bearerTokenEnv = initial.bearerTokenEnv;
  });
  $effect(() => {
    formError = initialError;
  });

  function recordToLines(record: Record<string, string>): string {
    return Object.entries(record)
      .map(([k, v]) => `${k}=${v}`)
      .join('\n');
  }

  function parseArgs(text: string): string[] {
    return text
      .split(/\s+/)
      .map((s) => s.trim())
      .filter((s) => s.length > 0);
  }

  function parseRecord(text: string): { ok: true; value: Record<string, string> } | { ok: false; error: string } {
    const out: Record<string, string> = {};
    const lines = text.split('\n');
    for (const raw of lines) {
      const line = raw.trim();
      if (!line) continue;
      const eqIdx = line.indexOf('=');
      if (eqIdx < 0) {
        return { ok: false, error: `Invalid entry "${line}" — expected key=value` };
      }
      const key = line.slice(0, eqIdx).trim();
      const value = line.slice(eqIdx + 1);
      if (!key) {
        return { ok: false, error: `Entry "${line}" has an empty key` };
      }
      out[key] = value;
    }
    return { ok: true, value: out };
  }

  // Both providers accept the same transport set for stdio. For HTTP
  // Claude uses "http"/"sse" and Codex uses "streamable_http". Map the
  // dropdown choice to a provider-native value at submit.
  function nativeTransport(p: ProviderKind, t: Transport): Transport {
    if (t === 'stdio') return 'stdio';
    if (p === 'codex') return 'streamable_http';
    if (t === 'streamable_http') return 'http';
    return t;
  }

  function validateClient(): string | null {
    if (!name.trim()) return 'Name is required.';
    if (!/^[A-Za-z0-9._-]+$/.test(name.trim())) {
      return 'Name must use letters, digits, dot, underscore, or hyphen only.';
    }
    if (name.includes('__')) {
      return 'Name cannot contain consecutive underscores.';
    }
    if (transport === 'stdio') {
      if (!command.trim()) return 'Command is required for stdio transport.';
    } else {
      if (!url.trim()) return 'URL is required for http/sse transport.';
      try {
        const parsed = new URL(url.trim());
        if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
          return 'URL must start with http:// or https://.';
        }
      } catch {
        return 'URL is not a valid URL.';
      }
    }
    return null;
  }

  function handleSubmit(): void {
    const err = validateClient();
    if (err) {
      formError = err;
      return;
    }
    const envParsed = parseRecord(envText);
    if (!envParsed.ok) {
      formError = `Env: ${envParsed.error}`;
      return;
    }
    const headersParsed = parseRecord(headersText);
    if (!headersParsed.ok) {
      formError = `Headers: ${headersParsed.error}`;
      return;
    }
    formError = null;
    onSubmit({
      provider,
      name: name.trim(),
      transport: nativeTransport(provider, transport),
      command: command.trim(),
      args: parseArgs(argsText),
      env: envParsed.value,
      url: url.trim(),
      headers: headersParsed.value,
      bearerTokenEnv: bearerTokenEnv.trim(),
    });
  }

  let isHttpish = $derived(transport !== 'stdio');
</script>

<div class="rounded-[var(--radius-card)] border border-border-subtle bg-surface-1/60 p-4">
  <div class="grid grid-cols-1 gap-3">
    <label class="flex flex-col gap-1">
      <span class="text-[11px] font-medium uppercase tracking-[0.12em] text-fg-hint">Provider</span>
      <select
        bind:value={provider}
        class={SELECT_CLASS}
        disabled={saving || mode === 'edit'}
        data-testid="mcp-form-provider"
      >
        <option value="claude">Claude Code</option>
        <option value="codex">Codex</option>
      </select>
      <span class="text-[11px] text-fg-subtle">
        Servers are written to the provider's native config file
        (Claude: <code>~/.claude.json</code>; Codex: <code>~/.codex/config.toml</code>).
      </span>
    </label>

    <label class="flex flex-col gap-1">
      <span class="text-[11px] font-medium uppercase tracking-[0.12em] text-fg-hint">Name</span>
      <input
        type="text"
        bind:value={name}
        placeholder="github"
        class={INPUT_CLASS}
        disabled={saving || mode === 'edit'}
        data-testid="mcp-form-name"
      />
      {#if mode === 'add'}
        <span class="text-[11px] text-fg-subtle">
          Tools will appear as <code>mcp__{name || '&lt;name&gt;'}__&lt;tool&gt;</code>.
          Letters, digits, dot, underscore, hyphen.
        </span>
      {/if}
    </label>

    <label class="flex flex-col gap-1">
      <span class="text-[11px] font-medium uppercase tracking-[0.12em] text-fg-hint">Transport</span>
      <select
        bind:value={transport}
        class={SELECT_CLASS}
        disabled={saving || mode === 'edit'}
        data-testid="mcp-form-transport"
      >
        <option value="stdio">stdio (local subprocess)</option>
        {#if provider === 'codex'}
          <option value="streamable_http">streamable_http</option>
        {:else}
          <option value="http">http (streamable HTTP)</option>
          <option value="sse">sse (Server-Sent Events)</option>
        {/if}
      </select>
    </label>

    {#if transport === 'stdio'}
      <label class="flex flex-col gap-1">
        <span class="text-[11px] font-medium uppercase tracking-[0.12em] text-fg-hint">Command</span>
        <input
          type="text"
          bind:value={command}
          placeholder="npx"
          class={INPUT_CLASS}
          disabled={saving}
          data-testid="mcp-form-command"
        />
      </label>
      <label class="flex flex-col gap-1">
        <span class="text-[11px] font-medium uppercase tracking-[0.12em] text-fg-hint">Arguments</span>
        <input
          type="text"
          bind:value={argsText}
          placeholder="-y @modelcontextprotocol/server-everything"
          class={INPUT_CLASS}
          disabled={saving}
          data-testid="mcp-form-args"
        />
        <span class="text-[11px] text-fg-subtle">Space-separated.</span>
      </label>
      <label class="flex flex-col gap-1">
        <span class="text-[11px] font-medium uppercase tracking-[0.12em] text-fg-hint">Environment</span>
        <textarea
          bind:value={envText}
          placeholder={'GITHUB_TOKEN=${GITHUB_TOKEN}'}
          rows={3}
          class={`${INPUT_CLASS} font-mono`}
          disabled={saving}
          data-testid="mcp-form-env"
        ></textarea>
        <span class="text-[11px] text-fg-subtle">
          One per line as <code>KEY=VALUE</code>. Use <code>${'$'}{'{'}VAR{'}'}</code> to
          reference your shell environment — secrets never touch AO storage.
        </span>
      </label>
    {:else if isHttpish}
      <label class="flex flex-col gap-1">
        <span class="text-[11px] font-medium uppercase tracking-[0.12em] text-fg-hint">URL</span>
        <input
          type="text"
          bind:value={url}
          placeholder="https://example.com/mcp"
          class={INPUT_CLASS}
          disabled={saving}
          data-testid="mcp-form-url"
        />
      </label>
      <label class="flex flex-col gap-1">
        <span class="text-[11px] font-medium uppercase tracking-[0.12em] text-fg-hint">Headers</span>
        <textarea
          bind:value={headersText}
          placeholder="X-Foo=bar"
          rows={3}
          class={`${INPUT_CLASS} font-mono`}
          disabled={saving}
          data-testid="mcp-form-headers"
        ></textarea>
        <span class="text-[11px] text-fg-subtle">One per line as <code>KEY=VALUE</code>.</span>
      </label>
      <label class="flex flex-col gap-1">
        <span class="text-[11px] font-medium uppercase tracking-[0.12em] text-fg-hint">Bearer env var</span>
        <input
          type="text"
          bind:value={bearerTokenEnv}
          placeholder="GITHUB_TOKEN"
          class={INPUT_CLASS}
          disabled={saving}
          data-testid="mcp-form-bearer-env"
        />
        <span class="text-[11px] text-fg-subtle">
          Name of the environment variable holding the bearer token. The provider
          reads it at spawn time; AO never sees the token value.
        </span>
      </label>
    {/if}
  </div>

  {#if formError}
    <p class="mt-3 text-[12px] text-error" data-testid="mcp-form-error">{formError}</p>
  {/if}

  <div class="mt-4 flex items-center justify-end gap-2">
    <button type="button" onclick={onCancel} class={SECONDARY_BUTTON_CLASS} disabled={saving}>
      Cancel
    </button>
    <button
      type="button"
      onclick={handleSubmit}
      class={PRIMARY_BUTTON_CLASS}
      disabled={saving}
      data-testid="mcp-form-submit"
    >
      {mode === 'add' ? 'Add server' : 'Save changes'}
    </button>
  </div>
</div>
