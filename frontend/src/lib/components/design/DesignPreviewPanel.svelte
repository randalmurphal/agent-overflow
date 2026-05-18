<script lang="ts">
  // DesignPreviewPanel — the main preview iframe in design threads.
  //
  // Owns the toolbar (viewport selector, refresh) and the sandbox iframe
  // that loads `/design/{threadId}/main/?cb={n}` from the Go file server
  // mounted on the same transport as the binding RPCs. The iframe is
  // `sandbox="allow-scripts"` only — never allow-same-origin — so the
  // agent-rendered HTML cannot reach into the host document. The Go
  // server injects a tiny capture script into the served HTML that
  // posts diagnostics back to the parent via window.postMessage; we
  // forward those to the backend via IngestDiagnosticBatch.
  //
  // Cache busting: when the file watcher fires `design:reload-main`
  // (re-dispatched as a throttled DOM event by `events.ts`), we bump
  // the cacheBust counter on the iframe src to force a reload.
  //
  // The user-facing "send to thread" button captures a single PNG of
  // the iframe via `requestIframeCapture` (utils/sendDesignToThread.ts
  // owns that pipeline). The agent's read_screenshot MCP tool is
  // backend-driven (chromedp / chrome-headless-shell renders the same
  // /design/ URL) and does not round-trip through this iframe.
  //
  // Sandbox + capture: with `sandbox="allow-scripts"` set without
  // `allow-same-origin`, the iframe document loads with an opaque
  // origin and the parent cannot reach `iframe.contentDocument`.
  // The send-to-thread capture works around that by sending a
  // postMessage `{aoDesign: 'capture', requestId, mode: 'single'}` to
  // the iframe; the file-server-injected capture script renders the
  // document via a lazy-loaded modern-screenshot module and posts back
  // a `capture-result`. utils/captureHtml.ts owns the messenger logic.
  // Diagnostics flow over the same postMessage rail (works across
  // opaque origins).

  import { onMount } from 'svelte';
  import Smartphone from 'lucide-svelte/icons/smartphone';
  import TabletIcon from 'lucide-svelte/icons/tablet';
  import Monitor from 'lucide-svelte/icons/monitor';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import MessagesSquare from 'lucide-svelte/icons/messages-square';
  import MessageSquarePlus from 'lucide-svelte/icons/message-square-plus';
  import type { PanelContext } from '../../stores/rhsPanelSlot.svelte';
  import {
    IngestDiagnosticBatch,
    EnsureDesignWorkdir,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { sendDesignToThread } from '../../utils/sendDesignToThread';
  import {
    DESIGN_VIEWPORT_WIDTHS,
    type Diagnostic,
    type DiagnosticSeverity,
    type DesignViewport,
  } from '../../types/design';
  import { DESIGN_RELOAD_MAIN_EVENT } from '../../stores/events';
  import Icon from '../primitives/Icon.svelte';

  let { ctx }: { ctx: PanelContext } = $props();

  let iframeEl: HTMLIFrameElement | undefined = $state(undefined);
  let cacheBust = $state(0);
  let sendingToThread = $state(false);
  let threadId = $derived(ctx.threadId);

  // workdirReadyForThread is the thread id whose {main,options}/ layout
  // has been confirmed via EnsureDesignWorkdir. We gate the iframe
  // `src` on this so a fresh thread that's never had
  // an agent session doesn't load /design/{threadId}/main/ before the
  // file server has anything to return — http.FileServer would 404,
  // and a 404 from the same origin as the SPA hits the asset
  // handler's X-Frame-Options: deny, leaving the iframe stuck on the
  // browser's chrome-error page. Idempotent on the backend so the
  // mount call is cheap when the workdir already exists.
  let workdirReadyForThread = $state<string | null>(null);

  let iframeSrc = $derived.by<string | null>(() => {
    if (!threadId) return null;
    if (workdirReadyForThread !== threadId) return null;
    return `/design/${encodeURIComponent(threadId)}/main/?cb=${cacheBust}`;
  });

  $effect(() => {
    const currentThreadId = threadId;
    if (!currentThreadId) {
      workdirReadyForThread = null;
      return;
    }
    if (workdirReadyForThread === currentThreadId) return;
    let cancelled = false;
    void EnsureDesignWorkdir(currentThreadId)
      .then(() => {
        if (cancelled) return;
        if (threadId !== currentThreadId) return;
        workdirReadyForThread = currentThreadId;
        // Re-derive picker state from disk after the workdir is
        // confirmed. Without this, a refresh / app restart drops the
        // in-memory activeOptionSet and leaves the user looking at
        // the empty main/ placeholder even though their pending
        // option set is still on disk under options/{setId}/.
        // refreshDesignOptions consults LatestDesignOptionSet,
        // which filters out picked sets via the .picked marker.
        void ctx.refreshDesignOptions(currentThreadId);
      })
      .catch((err) => {
        if (cancelled) return;
        addToast('error', `Design preview unavailable: ${errString(err)}`);
      });
    return () => {
      cancelled = true;
    };
  });

  // -- Viewport selector -----------------------------------------------

  const VIEWPORTS: ReadonlyArray<{
    value: DesignViewport;
    label: string;
    icon: typeof Smartphone;
  }> = [
    { value: 'mobile', label: 'Mobile', icon: Smartphone },
    { value: 'tablet', label: 'Tablet', icon: TabletIcon },
    { value: 'desktop', label: 'Desktop', icon: Monitor },
  ];

  let viewportWidthPx = $derived(DESIGN_VIEWPORT_WIDTHS[ctx.designViewport]);

  function selectViewport(next: DesignViewport) {
    ctx.setDesignViewport(next);
  }

  function refresh(): void {
    cacheBust += 1;
  }

  // -- Send to thread ---------------------------------------------------
  //
  // Click handler is a thin wrapper around `sendDesignToThread` in
  // utils/sendDesignToThread.ts — that helper owns the
  // capture → GetDesignWorkdirInfo → CreateThread → UploadAttachment →
  // SaveDraft → switch pipeline plus the orphan-thread rollback on
  // partial failure. Keeping the orchestration outside the component
  // honors the frontend AGENTS guidance to keep .svelte files focused
  // on rendering + input capture and keeps the file under the
  // ~300-line ceiling.

  async function onSendToThread(): Promise<void> {
    if (sendingToThread) return;
    const iframe = iframeEl;
    if (!iframe) {
      addToast('error', 'Send to thread: preview iframe not ready');
      return;
    }
    sendingToThread = true;
    try {
      await sendDesignToThread({ ctx, iframe });
    } finally {
      sendingToThread = false;
    }
  }

  // -- Diagnostic forwarding -------------------------------------------

  // Buffer postMessage diagnostics for 200ms and flush as one batch.
  // Burst-prone iframes (failed CDN loads, mermaid mid-render) emit
  // many entries per frame; the backend's per-thread ring tolerates
  // bursts but the wire round-trip cost is one method invocation per
  // batch — pay it once.
  const DIAG_DEBOUNCE_MS = 200;
  let diagBuffer: Diagnostic[] = [];
  let diagFlushHandle: ReturnType<typeof setTimeout> | null = null;

  function flushDiagnostics(): void {
    diagFlushHandle = null;
    const currentThreadId = threadId;
    const batch = diagBuffer;
    diagBuffer = [];
    if (!currentThreadId || batch.length === 0) return;
    // The wire shape is identical; the generated enum-typed class is a
    // type-only convenience. Cast the plain literal so we don't have to
    // construct the class for an outbound call.
    void IngestDiagnosticBatch(
      { threadId: currentThreadId, diagnostics: batch } as Parameters<typeof IngestDiagnosticBatch>[0],
    ).catch((err) => {
      console.warn('IngestDiagnosticBatch failed:', err);
    });
  }

  function isDiagnosticSeverity(value: unknown): value is DiagnosticSeverity {
    return value === 'error' || value === 'warn' || value === 'info';
  }

  function normalizeDiagnostic(raw: unknown): Diagnostic | null {
    if (!raw || typeof raw !== 'object') return null;
    const r = raw as Record<string, unknown>;
    if (typeof r.message !== 'string') return null;
    const severity = isDiagnosticSeverity(r.severity) ? r.severity : 'info';
    const now = Date.now();
    return {
      // Backend assigns the canonical token; the iframe value (if any)
      // is informational only. Send 0 and let the buffer stamp it.
      token: typeof r.token === 'number' ? r.token : 0,
      severity,
      message: r.message,
      source: typeof r.source === 'string' ? r.source : undefined,
      line: typeof r.line === 'number' ? r.line : undefined,
      column: typeof r.column === 'number' ? r.column : undefined,
      stack: typeof r.stack === 'string' ? r.stack : undefined,
      url: typeof r.url === 'string' ? r.url : undefined,
      createdAt: typeof r.createdAt === 'number' ? r.createdAt : now,
    };
  }

  function handlePostMessage(ev: MessageEvent): void {
    // Only trust messages whose source is the iframe we mounted. The
    // sandbox=allow-scripts iframe has an opaque origin so we can't
    // match by origin, but the contentWindow identity is still a
    // reliable comparator. Without this guard, any frame on the page
    // (or any other postMessage source) could spoof a diagnostic
    // batch and have it forwarded into the per-thread ring.
    if (!iframeEl || ev.source !== iframeEl.contentWindow) return;
    const data = ev.data;
    if (!data || typeof data !== 'object') return;
    if ((data as { aoDesign?: unknown }).aoDesign !== 'diagnostics') return;
    const items = (data as { items?: unknown }).items;
    if (!Array.isArray(items)) return;
    for (const raw of items) {
      const diag = normalizeDiagnostic(raw);
      if (diag) diagBuffer.push(diag);
    }
    if (diagBuffer.length === 0) return;
    if (diagFlushHandle === null) {
      diagFlushHandle = setTimeout(flushDiagnostics, DIAG_DEBOUNCE_MS);
    }
  }

  // -- Reload-main throttled subscription ------------------------------

  function handleReloadMain(ev: Event): void {
    const detail = (ev as CustomEvent).detail as { threadId?: string } | null;
    if (!detail?.threadId || detail.threadId !== threadId) return;
    cacheBust += 1;
  }

  onMount(() => {
    window.addEventListener('message', handlePostMessage);
    window.addEventListener(DESIGN_RELOAD_MAIN_EVENT, handleReloadMain);
    return () => {
      window.removeEventListener('message', handlePostMessage);
      window.removeEventListener(DESIGN_RELOAD_MAIN_EVENT, handleReloadMain);
      if (diagFlushHandle !== null) {
        clearTimeout(diagFlushHandle);
        diagFlushHandle = null;
      }
      // Send any buffered diagnostics on teardown so events captured
      // right before a thread switch don't get dropped.
      flushDiagnostics();
    };
  });
</script>

<div class="flex flex-col h-full min-h-0 bg-transparent">
  <!-- Toolbar — viewport switch · refresh · send-to-thread. -->
  <div
    class="flex items-center gap-2 border-b border-border-subtle px-3 py-2 shrink-0 min-w-0"
  >
    <div class="flex items-center gap-0.5 shrink-0">
      {#each VIEWPORTS as vp (vp.value)}
        <button
          type="button"
          onclick={() => selectViewport(vp.value)}
          aria-pressed={ctx.designViewport === vp.value}
          aria-label={vp.label}
          title={vp.label}
          class={[
            'inline-flex items-center justify-center rounded-[var(--radius-field)]',
            'p-1 cursor-pointer transition-colors',
            'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
            ctx.designViewport === vp.value
              ? 'bg-accent/15 text-fg'
              : 'text-fg-muted hover:bg-surface-2/30 hover:text-fg',
          ].join(' ')}
        >
          <Icon icon={vp.icon} size={14} strokeWidth={1.6} />
        </button>
      {/each}
    </div>

    <div class="h-4 w-px bg-border-subtle/60 shrink-0" aria-hidden="true"></div>

    <button
      type="button"
      onclick={refresh}
      disabled={!iframeSrc}
      aria-label="Refresh Preview"
      title="Refresh Preview"
      class={[
        'inline-flex items-center justify-center rounded-[var(--radius-field)]',
        'p-1 text-fg-muted cursor-pointer transition-colors',
        'hover:text-fg hover:bg-surface-2/30',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
        'disabled:opacity-50 disabled:cursor-not-allowed',
      ].join(' ')}
      data-testid="design-refresh"
    >
      <Icon icon={RefreshCw} size={13} strokeWidth={1.7} />
    </button>

    <button
      type="button"
      onclick={() => void onSendToThread()}
      disabled={!iframeSrc || sendingToThread}
      title="Open a new chat thread seeded with the design path + screenshot"
      aria-label="Send design to a new chat thread"
      class={[
        'inline-flex items-center gap-1 rounded-[var(--radius-field)] ml-auto',
        'border border-border-subtle bg-surface-0 px-2 py-1',
        'text-[12px] text-fg cursor-pointer transition-colors',
        'hover:border-border focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
        'disabled:opacity-60 disabled:cursor-not-allowed',
      ].join(' ')}
      data-testid="design-send-to-thread"
    >
      <Icon icon={MessageSquarePlus} size={12} strokeWidth={1.7} class="shrink-0" />
      <span>{sendingToThread ? 'Sending…' : 'Send to thread'}</span>
    </button>
  </div>

  <div class="flex-1 min-h-0 overflow-auto bg-surface-0/60 flex items-start justify-center p-2">
    {#if !threadId}
      <div class="flex flex-col items-center justify-center h-full text-center text-fg-muted">
        <Icon icon={MessagesSquare} size={36} strokeWidth={1.2} class="text-fg-hint mb-3" />
        <p class="text-[13px]">No Design Thread Loaded</p>
      </div>
    {:else if !iframeSrc}
      <div class="flex flex-col items-center justify-center h-full text-center text-fg-muted">
        <p class="text-[13px]">Preparing preview…</p>
      </div>
    {:else}
      <iframe
        bind:this={iframeEl}
        title="Design Preview"
        src={iframeSrc}
        sandbox="allow-scripts"
        referrerpolicy="no-referrer"
        class="h-full rounded-[var(--radius-field)] border border-border-subtle bg-white"
        style="width: {viewportWidthPx ? `${viewportWidthPx}px` : '100%'}; max-width: 100%;"
        data-testid="design-preview-iframe"
      ></iframe>
    {/if}
  </div>
</div>
