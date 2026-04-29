<script lang="ts">
  import { fade } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    GetProviderStatuses,
    ProbeClaudeAccount,
    ReconnectSession,
  } from '../../stores/bindings';
  import { userFacingError } from '../../utils/userFacingError';
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

  let sessionBannerVisible = $derived(!!pane.thread && (reconnecting || !!pane.generalError));
  let sessionBannerClasses = $derived(
    reconnecting
      ? 'bg-warning/15 border-warning/30 text-warning'
      : 'bg-error/15 border-error/30 text-error',
  );
  let sessionMessage = $derived(
    reconnecting ? 'Reconnecting…' : (pane.generalError ?? 'Provider error'),
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
        // "Install …" points to the install-docs URL supplied by Go. If
        // the backend somehow shipped an empty `actionUrl`, a click on
        // this button would be a silent no-op — hide the affordance
        // entirely rather than lie to the user.
        if (!providerStatus.actionUrl) return '';
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
    pane.clearGeneralError();
    try {
      await ReconnectSession(pane.threadId);
    } catch (err) {
      console.error('Failed to reconnect:', err);
      pane.setGeneralError(userFacingError(err));
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

  // Recheck is also exposed on `not_found` so a user who just ran
  // `npm i -g @anthropic-ai/claude-code` can clear the banner without
  // restarting the app. Re-running the binary detection emits the
  // refreshed provider:status which the store consumes — same path
  // the boot probe takes.
  async function handleRecheckBinary() {
    if (rechecking) return;
    rechecking = true;
    try {
      await GetProviderStatuses();
    } catch (err) {
      console.error('Failed to recheck provider status:', err);
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

<!--
  Reserved-slot pattern: each banner sits in a fixed-height wrapper so the
  banner showing/hiding never animates the height of the chat column. The
  scroll surface adjacent to the slots stays geometrically stable.

  Cost: ~36px reserved per slot whether or not a banner is shown. This is
  the right trade-off — banners signal system state (auth expired, provider
  missing, reconnecting) that the user benefits from seeing in a stable
  location, and the alternative (height-animated mount) shifts every visible
  message under the user's cursor.
-->
<div class="relative shrink-0 min-h-9">
  {#if providerStatus && pane.thread}
    <div
      transition:fade={{ duration: 150 }}
      role="alert"
      aria-live="polite"
      data-testid="provider-status-banner"
      data-status={providerStatus.status}
      class="border-b {providerBannerClasses} px-4 py-2 flex items-center gap-2"
    >
      <p class="text-xs flex-1 line-clamp-2" title={providerBannerMessage}>
        {providerBannerMessage}
      </p>
      {#if providerStatus.status === 'not_found'}
        <button
          onclick={handleRecheckBinary}
          disabled={rechecking}
          data-testid="provider-status-recheck"
          class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-white/5 cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          {rechecking ? 'Checking…' : 'Recheck'}
        </button>
      {/if}
      {#if providerStatus.actionable && primaryActionLabel}
        <button
          onclick={handlePrimaryAction}
          disabled={rechecking}
          data-testid="provider-status-action"
          class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-white/5 cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          {rechecking ? 'Checking…' : primaryActionLabel}
        </button>
      {/if}
    </div>
  {/if}
</div>

<div class="relative shrink-0 min-h-9">
  {#if sessionBannerVisible && pane.thread}
    <div transition:fade={{ duration: 150 }} role="alert" aria-live="assertive" class="border-b {sessionBannerClasses} px-4 py-2 flex items-center gap-2">
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
        onclick={() => pane.clearGeneralError()}
        class="text-xs hover:opacity-70 cursor-pointer shrink-0 px-1 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
        aria-label="Dismiss Banner"
      >
        Dismiss
      </button>
    </div>
  {/if}
</div>
