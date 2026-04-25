<script lang="ts">
  // RemoteEndpointRow renders one stored endpoint with its name + URL,
  // a token reveal toggle (masked by default), and the action buttons
  // (Copy connect command, Edit, Delete).
  //
  // Token fetch / reveal / clipboard write happens here so the parent
  // doesn't need a per-row state map; the row owns its own loading,
  // reveal, and copy-feedback state. Persistence + list mutation stays
  // with the parent (Add/Update/Delete bindings are list-scoped and
  // need to update the surrounding list state).
  import {
    GetRemoteEndpointToken,
    TouchRemoteEndpoint,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { buildLaunchCommand } from '../../utils/connectCommand';

  interface EndpointSummary {
    id: string;
    name: string;
    url: string;
    lastUsedAt?: number;
  }

  interface Props {
    endpoint: EndpointSummary;
    saving: boolean;
    onEdit: () => void;
    onDelete: () => void;
  }

  const { endpoint, saving, onEdit, onDelete }: Props = $props();

  // Per-row token state. Holds the actual revealed token (not just a
  // flag) so a re-render doesn't have to refetch. Cleared whenever the
  // parent rebuilds the row (the key in {#each} keeps this row's state
  // tied to the same endpoint id).
  let revealedToken = $state<string | null>(null);
  let copyState = $state<'idle' | 'copied' | 'failed'>('idle');
  let copyTimer: ReturnType<typeof setTimeout> | null = null;

  function setCopyState(state: 'idle' | 'copied' | 'failed'): void {
    copyState = state;
    if (copyTimer) clearTimeout(copyTimer);
    if (state !== 'idle') {
      copyTimer = setTimeout(() => {
        copyState = 'idle';
        copyTimer = null;
      }, 1500);
    } else {
      copyTimer = null;
    }
  }

  async function toggleReveal(): Promise<void> {
    if (revealedToken !== null) {
      revealedToken = null;
      return;
    }
    try {
      revealedToken = (await GetRemoteEndpointToken(endpoint.id)) as string;
    } catch (err) {
      addToast('error', `Failed to load token: ${errString(err)}`);
    }
  }

  async function copyLaunchCommand(): Promise<void> {
    let token: string;
    try {
      token = (await GetRemoteEndpointToken(endpoint.id)) as string;
    } catch {
      setCopyState('failed');
      return;
    }
    const cmd = buildLaunchCommand(endpoint.url, token);
    try {
      await navigator.clipboard.writeText(cmd);
      setCopyState('copied');
      // Best-effort: bump LastUsedAt so the list can sort / highlight
      // recently-used endpoints. Failure is harmless.
      try {
        await TouchRemoteEndpoint(endpoint.id);
      } catch {
        // Swallow: see comment above.
      }
    } catch {
      setCopyState('failed');
    }
  }

  // Mask is purely visual: the bulk list omits the real token, so the
  // length is unknown. Render a fixed-width ellipsis when masked
  // rather than peeking at unknown character counts.
  const MASK = '•••…•••';

  $effect(() => {
    return () => {
      if (copyTimer) clearTimeout(copyTimer);
    };
  });
</script>

<li class="flex flex-col gap-2 py-3">
  <div class="flex items-start justify-between gap-3">
    <div class="min-w-0 flex-1">
      <p class="text-[13px] font-medium text-fg truncate">
        {endpoint.name?.trim() || endpoint.url}
      </p>
      <p class="mt-0.5 text-[11px] text-fg-muted font-mono truncate">{endpoint.url}</p>
      <p class="mt-1 flex items-center gap-2 text-[11px] text-fg-muted font-mono">
        <span>Token:</span>
        <span aria-label="Token">
          {revealedToken ?? MASK}
        </span>
        <button
          type="button"
          onclick={toggleReveal}
          class="text-[11px] text-fg-muted hover:text-fg cursor-pointer underline-offset-2 hover:underline"
        >
          {revealedToken !== null ? 'Hide' : 'Show'}
        </button>
      </p>
    </div>
    <div class="flex shrink-0 gap-2">
      <button
        type="button"
        onclick={copyLaunchCommand}
        class="text-[12px] font-medium rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg hover:border-accent/40 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors cursor-pointer"
        aria-label="Copy connect command for {endpoint.name || endpoint.url}"
      >
        {#if copyState === 'copied'}
          Copied
        {:else if copyState === 'failed'}
          Copy failed
        {:else}
          Copy connect command
        {/if}
      </button>
      <button
        type="button"
        onclick={onEdit}
        class="text-[12px] font-medium rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg hover:border-accent/40 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors cursor-pointer"
      >
        Edit
      </button>
      <button
        type="button"
        onclick={onDelete}
        disabled={saving}
        class="text-[12px] font-medium rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg hover:border-error/50 hover:text-error disabled:opacity-50 disabled:cursor-not-allowed focus:outline-none focus-visible:ring-2 focus-visible:ring-error/40 transition-colors cursor-pointer"
      >
        Delete
      </button>
    </div>
  </div>
</li>
