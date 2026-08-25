<script lang="ts">
  import { fade } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    GetProviderStatuses,
    ReconnectSession,
  } from '../../stores/bindings';
  import { userFacingError } from '../../utils/userFacingError';
  import {
    getProviderStatus,
    recordProviderStatus,
    type ProviderStatusEvent,
  } from '../../stores/providerStatus.svelte';
  import { handleExternalURL } from '../../utils/externalLinks';
  import {
    recheckProviderAccount,
    recheckResultClearsAuthBanner,
  } from '../../providers/actions';
  import {
    getProviderDefinition,
    providerCliLabel,
  } from '../../providers/catalog';

  let { pane }: { pane: ThreadPane } = $props();

  let reconnecting = $state(false);
  let retryingHistory = $state(false);
  let rechecking = $state(false);

  // Provider-level status is keyed by the pane's current provider. When
  // the pane has no thread yet (boot, between switches) we stay empty.
  // `pane.providerBanner` is the chat-state mirror used by the rewrite;
  // the providerStatus store remains the app-wide cache so thread switches
  // can seed immediately from the last seen event for that provider.
  let providerStatus = $derived.by((): ProviderStatusEvent | null => {
    if (!pane.thread) return null;
    const evt = pane.providerBanner !== undefined
      ? pane.providerBanner
      : getProviderStatus(pane.thread.provider);
    if (!evt) return null;
    // Ready events are clear-banner signals — render nothing.
    return evt.status === 'ready' ? null : evt;
  });

  // One stacked row per stored error kind (session / history-load /
  // general), each with its own action and Dismiss — user ruling
  // 2026-08-25, replacing the single resolved slot whose no-clobber rule
  // silently hid secondary errors. A row in a busy state (reconnecting,
  // retrying) swaps to warning colours and progress copy in place.
  const ERROR_ROW_CLASSES = 'bg-error/15 border-error/30 text-error';
  const BUSY_ROW_CLASSES = 'bg-warning/15 border-warning/30 text-warning';
  function rowBusy(kind: string): boolean {
    return (
      (kind === 'session' && reconnecting) ||
      (kind === 'history-load' && retryingHistory)
    );
  }
  function rowMessage(kind: string, message: string): string {
    if (kind === 'session' && reconnecting) return 'Reconnecting…';
    if (kind === 'history-load' && retryingHistory) return 'Retrying thread history…';
    return message;
  }

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
        const label = providerCliLabel(providerStatus.provider);
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
        return getProviderDefinition(providerStatus.provider)?.installActionLabel ?? '';
      case 'unauthenticated':
        return 'Recheck';
      default:
        return '';
    }
  });

  async function handleReconnect() {
    if (!pane.threadId || reconnecting) return;
    reconnecting = true;
    try {
      await ReconnectSession(pane.threadId);
      // Only the session row resolves; an orthogonal error's row stays.
      pane.clearPaneError('session');
      // Re-pull from the backend even on success. The backend's
      // CleanupThread synthesizes a truncated provider:turn_completed
      // when triage state was live, but events that fire during the
      // round-trip can race the FE pane's reactive state. Refreshing
      // is the cheap belt-and-braces fix that matches the
      // transport-gap recovery path and clears any stale activeTurn /
      // streaming flags the user might still see.
      await pane.refreshFromBackend();
    } catch (err) {
      console.error('Failed to reconnect:', err);
      // Replace the session row's message so its Reconnect stays offered.
      pane.setPaneError(userFacingError(err), 'session');
    } finally {
      reconnecting = false;
    }
  }

  async function handleHistoryRetry() {
    if (retryingHistory) return;
    retryingHistory = true;
    try {
      await pane.retryHistoryLoad();
    } finally {
      retryingHistory = false;
    }
  }

  async function handleRecheckAuth() {
    const status = providerStatus;
    if (!status || rechecking) return;
    rechecking = true;
    try {
      // RecheckProviderAccount evicts the per-process probe cache before
      // re-running the probe — the cached pre-login zero-value would
      // otherwise mask the new auth state for up to 5 minutes.
      const account = await recheckProviderAccount(status.provider);
      if (recheckResultClearsAuthBanner(status.provider, account)) {
        const readyStatus: ProviderStatusEvent = {
          provider: status.provider,
          status: 'ready',
          message: '',
          actionable: false,
        };
        recordProviderStatus(readyStatus);
        pane.setProviderBanner(status.threadId ? null : undefined);
      }
    } catch (err) {
      console.error('Failed to recheck provider account:', err);
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
      void handleExternalURL(providerStatus.actionUrl);
    }
  }
</script>

<!--
  Overlay pattern: both status banners float over the top edge of the chat
  surface instead of reserving layout height. On the happy path nothing
  renders and the timeline sits flush under the header; when a banner shows
  it overlays the top of the message list without changing the scroller's
  clientHeight, so messages never reflow — the property the old reserved
  slots bought, except those held ~36px each (~72px of empty space under
  the header) forever. Anchored to the relative timeline container in
  ChatView, stacking both banners top-down; z-20 matches the
  composer overlay in that container. A shown banner covers the topmost
  rows — intended: scroll up, or dismiss/resolve the banner, to reveal them.
-->
<div
  class="absolute inset-x-0 top-0 z-20 flex flex-col"
  data-testid="provider-status-overlay"
>
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
          class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-fg/10 cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          {rechecking ? 'Checking…' : 'Recheck'}
        </button>
      {/if}
      {#if providerStatus.actionable && primaryActionLabel}
        <button
          onclick={handlePrimaryAction}
          disabled={rechecking}
          data-testid="provider-status-action"
          class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-fg/10 cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          {rechecking ? 'Checking…' : primaryActionLabel}
        </button>
      {/if}
    </div>
  {/if}

  {#if pane.thread}
    {#each pane.paneErrorList as err (err.kind)}
      <div
        transition:fade={{ duration: 150 }}
        role="alert"
        aria-live="assertive"
        data-testid="pane-error-banner"
        data-kind={err.kind}
        class="border-b {rowBusy(err.kind) ? BUSY_ROW_CLASSES : ERROR_ROW_CLASSES} px-4 py-2 flex items-center gap-2"
      >
        <p class="text-xs flex-1 line-clamp-2" title={rowMessage(err.kind, err.message)}>
          {rowMessage(err.kind, err.message)}
        </p>
        {#if err.kind === 'session'}
          <button
            onclick={handleReconnect}
            disabled={reconnecting}
            class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-fg/10 cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            {reconnecting ? 'Reconnecting...' : 'Reconnect'}
          </button>
        {:else if err.kind === 'history-load'}
          <button
            onclick={handleHistoryRetry}
            disabled={retryingHistory}
            class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-fg/10 cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            {retryingHistory ? 'Retrying…' : 'Retry'}
          </button>
        {/if}
        <button
          onclick={() => pane.clearPaneError(err.kind)}
          class="text-xs hover:opacity-70 cursor-pointer shrink-0 px-1 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
          aria-label="Dismiss Banner"
        >
          Dismiss
        </button>
      </div>
    {/each}
  {/if}
</div>
