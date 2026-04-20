<script lang="ts">
  import { slide } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { ReconnectSession, ProbeClaudeAccount } from '../../stores/bindings';
  import {
    getProviderStatus,
    type ProviderStatusEvent,
  } from '../../stores/providerStatus.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let reconnecting = $state(false);
  let rechecking = $state(false);

  // Provider-level status is keyed by the pane's current provider. When
  // the pane has no thread yet (boot, between switches) we stay empty.
  // `pane.providerBanner` is the chat-state mirror used by the rewrite;
  // the providerStatus store remains the app-wide cache so thread switches
  // can seed immediately from the last seen event for that provider.
  let providerStatus = $derived.by((): ProviderStatusEvent | null => {
    if (!pane.thread) return null;
    const evt = pane.providerBanner ?? getProviderStatus(pane.thread.provider);
    if (!evt) return null;
    // Ready events are clear-banner signals — render nothing.
    return evt.status === 'ready' ? null : evt;
  });

  let sessionBannerVisible = $derived(!!pane.thread && (reconnecting || !!pane.error));
  let sessionBannerClasses = $derived(
    reconnecting
      ? 'bg-warning/15 border-warning/30 text-warning'
      : 'bg-error/15 border-error/30 text-error',
  );
  let sessionMessage = $derived(
    reconnecting ? 'Reconnecting…' : (pane.error ?? 'Provider error'),
  );

  // Status-level banner (install / version / auth) — colour + copy are
  // derived off the status string. Docs link (when present) comes from
  // the Go payload so the frontend can't drift from the URL table.
  let providerBannerClasses = $derived.by(() => {
    if (!providerStatus) return '';
    switch (providerStatus.status) {
      case 'not_found':
      case 'error':
        return 'bg-error/15 border-error/30 text-error';
      case 'version_too_old':
      case 'unauthenticated':
        return 'bg-warning/15 border-warning/30 text-warning';
      default:
        return '';
    }
  });

  let providerBannerMessage = $derived.by(() => {
    if (!providerStatus) return '';
    switch (providerStatus.status) {
      case 'not_found': {
        // Message from Go already names the binary path; the UI decorates
        // with a provider-specific CTA. Fall back to a generic copy when
        // the backend somehow ships an empty message.
        const label = providerStatus.provider === 'claude' ? 'Claude CLI' : 'Codex CLI';
        return providerStatus.message
          || `${label} not found at the configured path.`;
      }
      case 'version_too_old':
      case 'unauthenticated':
      case 'error':
        return providerStatus.message || 'Provider status error';
      default:
        return providerStatus.message ?? '';
    }
  });

  let primaryActionLabel = $derived.by(() => {
    if (!providerStatus) return '';
    switch (providerStatus.status) {
      case 'not_found':
        return providerStatus.provider === 'claude' ? 'Install Claude CLI' : 'Install Codex CLI';
      case 'unauthenticated':
        return 'Recheck';
      default:
        return '';
    }
  });

  async function handleReconnect() {
    if (!pane.threadId || reconnecting) return;
    reconnecting = true;
    pane.clearError();
    try {
      await ReconnectSession(pane.threadId);
    } catch (err) {
      console.error('Failed to reconnect:', err);
      pane.setError(`Failed to reconnect: ${err}`);
    } finally {
      reconnecting = false;
    }
  }

  async function handleRecheckAuth() {
    if (rechecking) return;
    rechecking = true;
    try {
      // ProbeClaudeAccount re-emits provider:status on its own (both on
      // a successful auth result and on the unauthenticated case), so we
      // just trigger it — the store update happens through the event bus.
      await ProbeClaudeAccount();
    } catch (err) {
      console.error('Failed to probe Claude account:', err);
    } finally {
      rechecking = false;
    }
  }

  function handlePrimaryAction() {
    if (!providerStatus) return;
    if (providerStatus.status === 'unauthenticated') {
      void handleRecheckAuth();
      return;
    }
    if (providerStatus.actionUrl) {
      window.open(providerStatus.actionUrl, '_blank', 'noopener,noreferrer');
    }
  }
</script>

{#if providerStatus && pane.thread}
  <div
    transition:slide={{ duration: 150 }}
    role="alert"
    aria-live="polite"
    data-testid="provider-status-banner"
    data-status={providerStatus.status}
    class="border-b {providerBannerClasses} px-4 py-2 flex items-center gap-2 shrink-0"
  >
    <p class="text-xs flex-1 line-clamp-2" title={providerBannerMessage}>
      {providerBannerMessage}
    </p>
    {#if providerStatus.actionable && primaryActionLabel}
      <button
        onclick={handlePrimaryAction}
        disabled={rechecking}
        data-testid="provider-status-action"
        class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-white/5 cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        {rechecking ? 'Checking...' : primaryActionLabel}
      </button>
    {/if}
  </div>
{/if}

{#if sessionBannerVisible && pane.thread}
  <div transition:slide={{ duration: 150 }} role="alert" aria-live="assertive" class="border-b {sessionBannerClasses} px-4 py-2 flex items-center gap-2 shrink-0">
    <p class="text-xs flex-1 line-clamp-2" title={sessionMessage}>{sessionMessage}</p>
    {#if !reconnecting}
      <button
        onclick={handleReconnect}
        disabled={reconnecting}
        class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-white/5 cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        {reconnecting ? 'Reconnecting...' : 'Reconnect'}
      </button>
    {/if}
    <button
      onclick={() => pane.clearError()}
      class="text-xs hover:opacity-70 cursor-pointer shrink-0 px-1 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
      aria-label="Dismiss banner"
    >
      Dismiss
    </button>
  </div>
{/if}
