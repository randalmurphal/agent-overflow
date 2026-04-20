import { Events } from '@wailsio/runtime';
import type { ApprovalEvent, ProviderStatusEvent, UsageEvent } from '../types/events';
import type { Item, Thread } from '../types/models';
import type { DesignArtifact, DesignChoiceResolved, DesignOptionsRequest } from '../types/design';
import { getAllPanes } from './panes.svelte';
import { addToast } from './toast.svelte';
import { getThreads, replaceThread } from './threads.svelte';
import { projectApprovalRequest, projectApprovalResolution, projectThreadItem } from './threadStatuses.svelte';

/**
 * SeqEnvelope mirrors the Go-side `SeqEnvelope` in app.go. Every Wails
 * emission stamps a monotonic `seq` so subscribers can detect gaps
 * (scaffolding for future remote-access transport). The envelope also
 * carries a `data` field with the original Go payload.
 *
 * We keep the detection shape duck-typed instead of using `instanceof`
 * or a JSON-schema check — the Go runtime serialises the envelope
 * through encoding/json, so what arrives in the webview is a plain
 * object with `seq` + `data` keys.
 */
interface SeqEnvelope<T = unknown> {
  seq: number;
  data: T;
}

function isSeqEnvelope(value: unknown): value is SeqEnvelope {
  return (
    value !== null
    && typeof value === 'object'
    && 'seq' in value
    && typeof (value as { seq: unknown }).seq === 'number'
    && 'data' in value
  );
}

/**
 * Per-event-name last-seen seq table. Keys are event names; values are
 * the most recent seq observed on that channel. Undefined = never seen,
 * so the first emission on a channel does not warn.
 *
 * We reset the table when setupEventListeners() is called again — tests
 * re-install listeners between cases, and a stale last-seen would
 * trigger spurious gap warnings.
 */
const lastSeenSeq: Map<string, number> = new Map();

function resetSeqTracking(): void {
  lastSeenSeq.clear();
}

/**
 * wailsEventOn wraps Events.On to (a) unwrap SeqEnvelope payloads back
 * to the original Go shape, and (b) track per-channel seq gaps. The
 * wrapper is robust against raw payloads: if `ev.data` is not a seq
 * envelope (older emits during rollout, tests driving raw data), the
 * handler receives the raw payload and no gap tracking runs.
 *
 * Exported so subscribers outside this file (terminal drawer, diff
 * panel) can share the same gap-detection + unwrapping logic without
 * re-implementing the boilerplate.
 */
export function wailsEventOn<T = unknown>(
  name: string,
  handler: (data: T) => void,
): () => void {
  return Events.On(name, (ev) => {
    const raw = ev.data as unknown;
    if (isSeqEnvelope(raw)) {
      const prev = lastSeenSeq.get(name);
      // A strictly-increasing seq means no gap; anything else (drop,
      // retransmit, out-of-order) produces a warn with the missing
      // range. We warn once per gap and still deliver the event — the
      // seq is an observability scaffold, not a back-pressure knob.
      if (prev !== undefined && raw.seq > prev + 1) {
        const missingCount = raw.seq - prev - 1;
        const firstMissing = prev + 1;
        const lastMissing = raw.seq - 1;
        console.warn(
          `event seq gap on ${name}: missing ${missingCount} event(s) ` +
          `(ids ${firstMissing}..${lastMissing})`,
        );
      }
      // Track the highest seq we've seen so a stale re-delivery doesn't
      // roll the pointer backward.
      if (prev === undefined || raw.seq > prev) {
        lastSeenSeq.set(name, raw.seq);
      }
      handler(raw.data as T);
      return;
    }
    // Raw payload: pass through unchanged so legacy emit callers and
    // tests driving raw data continue to work without special casing.
    handler(raw as T);
  });
}

/**
 * Payload for the backend-emitted thread:mode_changed event. Mirrors
 * ThreadModeChangedEvent in app_thread_interaction_mode.go.
 */
interface ModeChangedPayload {
  threadId: string;
  mode: NonNullable<Thread['mode']>;
  needsReconnect: boolean;
}

/**
 * Payload for thread:runtime_mode_changed — emitted whenever
 * SetThreadRuntimeMode persists a change. NeedsReconnect means the backend
 * is already restarting the active session; the frontend just refreshes
 * its cached thread row and surfaces a toast via the composer toolbar's
 * AccessToggle / the settings flow.
 */
interface RuntimeModeChangedPayload {
  threadId: string;
  runtimeMode: Thread['runtimeMode'];
  needsReconnect: boolean;
}

function syncThreadRow(updated: Thread): void {
  replaceThread(updated);
  for (const pane of getAllPanes().values()) {
    if (pane.threadId === updated.id && pane.thread) {
      pane.replaceThread(updated);
    }
  }
}

function updateThreadUsageCache(threadId: string, raw: string): void {
  const existing = getThreads().find((thread) => thread.id === threadId);
  if (existing) {
    replaceThread({ ...existing, lastTokenUsage: raw });
  }
  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== threadId || !pane.thread) continue;
    pane.replaceThread({ ...pane.thread, lastTokenUsage: raw });
  }
}

function applyApprovalEvent(evt: ApprovalEvent): void {
  if (!evt) return;

  if (evt.action === 'request' && evt.request?.threadId) {
    projectApprovalRequest(evt.request.threadId, evt.request.requestId);
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === evt.request.threadId) {
        pane.addApproval(evt.request);
      }
    }
    return;
  }

  if (evt.action === 'resolve' && evt.requestId) {
    projectApprovalResolution(evt.threadId, evt.requestId);
    for (const pane of getAllPanes().values()) {
      if (evt.threadId && pane.threadId !== evt.threadId) continue;
      const hadApproval = pane.pendingApprovals.some((approval) => approval.requestId === evt.requestId);
      if (!hadApproval) continue;
      pane.removeApproval(evt.requestId);
    }
  }
}

function applyUsageEvent(evt: UsageEvent): void {
  if (!evt?.threadId) return;
  const payload = evt.action === 'usage'
    ? {
        usedTokens: evt.usedTokens ?? 0,
        maxTokens: evt.maxTokens,
        usedPercentage: evt.contextPercent,
      }
    : null;

  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== evt.threadId) continue;
    if (payload) {
      pane.setContextWindow(payload);
    } else {
      pane.clearContextWindow();
    }
  }

  updateThreadUsageCache(
    evt.threadId,
    payload
      ? JSON.stringify({
          usedTokens: payload.usedTokens,
          maxTokens: payload.maxTokens,
          contextPercent: payload.usedPercentage,
        })
      : '',
  );
}

function applyItemUpsert(item: Item): void {
  if (!item || !item.threadId) return;
  projectThreadItem(item);

  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== item.threadId) continue;
    pane.upsertItem(item);
  }
}

function applyProviderStatus(evt: ProviderStatusEvent): void {
  if (!evt?.provider || !evt.status) return;
  const banner = evt.status === 'ready' ? null : evt;
  for (const pane of getAllPanes().values()) {
    if (pane.thread?.provider === evt.provider) {
      pane.setProviderBanner(banner);
    }
  }
}

function applyThreadUpdated(updated: Thread): void {
  if (!updated?.id) return;
  syncThreadRow(updated);
}

/**
 * Set up the app's Wails event listeners.
 * Returns a cleanup function that removes all listeners.
 */
export function setupEventListeners(): () => void {
  // Reset the gap-detection table so a previous setupEventListeners call
  // (tests re-wire between cases) does not bleed its last-seen seq into
  // the new listener set and trigger spurious warnings.
  resetSeqTracking();

  const cancelApproval = wailsEventOn<ApprovalEvent>('provider:approval', applyApprovalEvent);

  const cancelUsage = wailsEventOn<UsageEvent>('provider:usage', applyUsageEvent);

  const cancelProviderStatus = wailsEventOn<ProviderStatusEvent>('provider:status', applyProviderStatus);

  // provider:item_upsert — backend persisted (or updated) a timeline
  // item. Triage emits this for every tool_call lifecycle row + every
  // background tool_completion sibling so the frontend can show the
  // row inline as soon as it lands, without waiting for the
  // turn-complete ListItems reconcile.
  const cancelItemUpsert = wailsEventOn<Item>('provider:item_upsert', applyItemUpsert);

  const cancelThreadUpdated = wailsEventOn<Thread>('thread:updated', applyThreadUpdated);

  // design:artifact — a new rendered artifact. Append to the owning pane's
  // history. The preview panel auto-tracks the latest unless the user has
  // pinned a specific artifact via the dropdown.
  const cancelDesignArtifact = wailsEventOn<DesignArtifact>('design:artifact', (artifact) => {
    if (!artifact || !artifact.threadId) return;
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === artifact.threadId) {
        pane.appendDesignArtifact(artifact);
      }
    }
  });

  // design:options — agent blocked on present_options. Also append the option
  // artifacts to history so the picker thumbnails resolve without a round-trip.
  const cancelDesignOptions = wailsEventOn<DesignOptionsRequest>('design:options', (request) => {
    if (!request || !request.threadId) return;
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === request.threadId) {
        pane.setDesignOptions(request);
      }
    }
  });

  // design:chosen — user picked an option, backend resolved. Clear the
  // pending-options state. The corresponding artifact stays in history.
  const cancelDesignChosen = wailsEventOn<DesignChoiceResolved>('design:chosen', (resolved) => {
    if (!resolved || !resolved.threadId) return;
    for (const pane of getAllPanes().values()) {
      if (pane.threadId !== resolved.threadId) continue;
      const current = pane.pendingDesignOptions;
      // Only clear if this resolution matches the currently-pending request.
      // A stale `chosen` event for an older request shouldn't wipe a newer
      // pending picker.
      if (current && current.requestId === resolved.requestId) {
        pane.clearDesignOptions();
      }
    }
  });

  // thread:runtime_mode_changed — backend persisted a new three-tier
  // approval mode. Refresh the sidebar cache and active pane; the backend
  // kicks off a session reconnect itself when needed, so the frontend just
  // needs to keep its thread shape in sync (AccessToggle's own optimistic
  // update already covered the pane that triggered the change).
  const cancelRuntimeModeChanged = wailsEventOn<RuntimeModeChangedPayload>(
    'thread:runtime_mode_changed',
    (payload) => {
      if (!payload || !payload.threadId || !payload.runtimeMode) return;
      const existing = getThreads().find((t) => t.id === payload.threadId);
      if (existing) {
        replaceThread({ ...existing, runtimeMode: payload.runtimeMode });
      }
      for (const pane of getAllPanes().values()) {
        if (pane.threadId !== payload.threadId) continue;
        if (pane.thread) {
          pane.replaceThread({ ...pane.thread, runtimeMode: payload.runtimeMode });
        }
      }
    },
  );

  // thread:mode_changed — the backend persisted a new mode. We update the
  // cached thread row (so sidebar badges refresh) and, when the change
  // landed on an active session, surface a toast prompting the user to
  // reconnect so the session can pick up the new mode's config.
  const cancelModeChanged = wailsEventOn<ModeChangedPayload>(
    'thread:mode_changed',
    (payload) => {
      if (!payload || !payload.threadId) return;
      const existing = getThreads().find((t) => t.id === payload.threadId);
      if (existing) {
        replaceThread({ ...existing, mode: payload.mode });
      }
      for (const pane of getAllPanes().values()) {
        if (pane.threadId !== payload.threadId) continue;
        if (pane.thread) {
          pane.replaceThread({ ...pane.thread, mode: payload.mode });
        }
      }
      if (payload.needsReconnect) {
        addToast(
          'warning',
          `Mode set to ${payload.mode}. Reconnect the session to apply.`,
        );
      }
    },
  );

  return () => {
    cancelApproval();
    cancelUsage();
    cancelProviderStatus();
    cancelItemUpsert();
    cancelThreadUpdated();
    cancelDesignArtifact();
    cancelDesignOptions();
    cancelDesignChosen();
    cancelModeChanged();
    cancelRuntimeModeChanged();
  };
}
