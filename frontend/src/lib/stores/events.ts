import { Events } from '@wailsio/runtime';
import type { ProviderEvent, ApprovalRequest, ContextWindow, RateLimitEntry, TokenUsage, ToolProgressMeta } from '../types/events';
import type { PayloadMeta, Thread } from '../types/models';
import type { DesignArtifact, DesignChoiceResolved, DesignOptionsRequest } from '../types/design';
import type { ThreadPane } from './thread.svelte';
import { getAllPanes } from './panes.svelte';
import { addToast } from './toast.svelte';
import { getThreads, updateThreadTitle, updateThreadModel, replaceThread } from './threads.svelte';
import { RespondToApproval, ApprovalResponse } from './bindings';

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
 * Payload for the backend-emitted thread:interaction_mode_changed event. Mirrors
 * ThreadInteractionModeChangedEvent in app_thread_interaction_mode.go.
 */
interface InteractionModeChangedPayload {
  threadId: string;
  interactionMode: Thread['interactionMode'];
  needsReconnect: boolean;
}

/**
 * Payload for thread:runtime_mode_changed — emitted whenever
 * SetThreadRuntimeMode persists a change. NeedsReconnect means the backend
 * is already restarting the active session; the frontend just refreshes
 * its cached thread row and surfaces a toast via RuntimeModePicker /
 * settings flow.
 */
interface RuntimeModeChangedPayload {
  threadId: string;
  runtimeMode: Thread['runtimeMode'];
  needsReconnect: boolean;
}

/**
 * Route a provider event to the correct pane mutation.
 * Called once per pane that matches the event's threadId.
 *
 * Every EventKind has an explicit case -- nothing falls through to default.
 */
function routeEventToPane(pane: ThreadPane, evt: ProviderEvent): void {
  switch (evt.kind) {
    case 'text_delta':
      pane.appendTextDelta(evt.content ?? '');
      break;

    case 'tool_start':
      pane.addToolCall(evt.itemId ?? '', evt.meta);
      break;

    case 'tool_complete':
      // Don't fabricate an item -- just mark the tool call done.
      // The persisted item arrives via finalizeTurn's DB reload.
      pane.completeToolCall(evt.itemId ?? '');
      break;

    case 'turn_start':
      pane.setSessionStatus('running');
      break;

    case 'turn_complete':
      pane.setSessionStatus('ready');
      pane.finalizeTurn();
      break;

    case 'approval_request':
      if (evt.meta) {
        const approval = evt.meta as ApprovalRequest;
        if (pane.isToolSessionApproved(approval.toolName)) {
          const threadId = pane.threadId;
          if (threadId) {
            RespondToApproval(threadId, new ApprovalResponse({
              requestId: approval.requestId,
              decision: 'allow',
            })).catch((err) => {
              console.error('Failed to auto-resolve approval:', err);
              addToast('error', `Auto-approval failed for ${approval.toolName}`);
              pane.addApproval(approval);
            });
          }
        } else {
          pane.addApproval(approval);
        }
      }
      break;

    case 'approval_resolved':
      if (evt.itemId) {
        pane.removeApproval(evt.itemId);
      }
      break;

    case 'session_status':
      pane.setSessionStatus(evt.content ?? 'unknown');
      break;

    case 'token_usage':
      if (evt.meta) {
        pane.setTokenUsage(evt.meta as TokenUsage);
      }
      break;

    case 'error':
      console.error('Provider error:', evt.content);
      pane.setSessionStatus('error');
      pane.setError(evt.content ?? 'Unknown provider error');
      break;

    case 'init':
      pane.setSessionStatus('connected');
      break;

    case 'background_start':
      pane.addBackgroundTask(evt.itemId ?? '', evt.meta);
      break;

    case 'background_delta':
      // Background deltas are accumulated server-side. No frontend action needed.
      break;

    case 'background_complete':
      pane.completeBackgroundTask(evt.itemId ?? '');
      break;

    case 'diff':
      // Diff events are persisted as heavy payloads by the backend.
      // The item and payload meta arrive separately. Nothing to do inline.
      break;

    case 'command_output':
      // Command output events are persisted as heavy payloads by the backend.
      // The item and payload meta arrive separately. Nothing to do inline.
      break;

    case 'thinking':
      // Thinking events are persisted as heavy payloads by the backend.
      // The item and payload meta arrive separately. Nothing to do inline.
      break;

    case 'tool_progress':
      if (evt.itemId && evt.meta) {
        pane.updateToolProgress(evt.itemId, evt.meta as ToolProgressMeta);
      }
      break;

    case 'compact_boundary':
      if (evt.meta) {
        pane.setContextWindow(evt.meta as ContextWindow);
      }
      break;

    case 'rate_limits':
      if (evt.meta) {
        const data = evt.meta as { limits: RateLimitEntry[] };
        pane.setRateLimits(data.limits ?? []);
      }
      break;

    case 'model_rerouted':
      if (evt.meta) {
        const data = evt.meta as { newModel: string };
        pane.updateModel(data.newModel);
        updateThreadModel(evt.threadId, data.newModel);
        addToast('info', `Model rerouted to ${data.newModel}`);
      }
      break;

    case 'thread_renamed':
      if (evt.meta) {
        const data = evt.meta as { newTitle: string };
        pane.updateTitle(data.newTitle);
        updateThreadTitle(evt.threadId, data.newTitle);
      }
      break;

    case 'plan_update':
      // Stream the Codex turn/plan/updated payload onto the pane so the
      // PlanSidebar / follow-up banner can render an in-progress plan
      // without waiting for the finalized item/completed (itemType=plan)
      // event. Opaque JSON — consumers read `meta` and render the plan
      // shape themselves.
      if (evt.meta) {
        pane.setPendingPlanUpdate(evt.meta);
      }
      break;

    default: {
      // Exhaustiveness guard: if TypeScript complains on this line, a new
      // EventKind was added to types/events.ts without a matching case above.
      // Add one — don't rely on the default to silently swallow it. The cast
      // to `never` forces a compile error when the union is extended.
      const _exhaustive: never = evt.kind;
      void _exhaustive;
      break;
    }
  }
}

/**
 * Set up Wails event listeners for provider events.
 * Returns a cleanup function that removes all listeners.
 */
export function setupEventListeners(): () => void {
  // Reset the gap-detection table so a previous setupEventListeners call
  // (tests re-wire between cases) does not bleed its last-seen seq into
  // the new listener set and trigger spurious warnings.
  resetSeqTracking();

  const cancelEvent = wailsEventOn<ProviderEvent>('provider:event', (evt) => {
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === evt.threadId) {
        routeEventToPane(pane, evt);
      }
    }
  });

  const cancelMeta = wailsEventOn<PayloadMeta>('provider:meta', (meta) => {
    for (const pane of getAllPanes().values()) {
      // Only push meta to the pane that owns this thread.
      // If the backend includes a threadId, filter by it.
      // If threadId is absent (legacy), fall back to broadcasting to all panes.
      if (meta.threadId && pane.threadId !== meta.threadId) {
        continue;
      }
      pane.addPayloadMeta(meta);
    }
  });

  const cancelError = wailsEventOn<ProviderEvent>('provider:error', (evt) => {
    console.error('Provider error event:', evt.content);
    addToast('error', evt.content ?? 'Provider error');
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === evt.threadId) {
        pane.setError(evt.content ?? 'Unknown provider error');
      }
    }
  });

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
  // needs to keep its thread shape in sync (the RuntimeModePicker's own
  // optimistic update already covered the pane that triggered the change).
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

  // thread:interaction_mode_changed — the backend persisted a new mode. We
  // update the cached thread row (so sidebar badges refresh) and, when the
  // change landed on an active session, surface a toast prompting the user
  // to reconnect so the session can pick up the new mode's config.
  const cancelModeChanged = wailsEventOn<InteractionModeChangedPayload>(
    'thread:interaction_mode_changed',
    (payload) => {
      if (!payload || !payload.threadId) return;
      const existing = getThreads().find((t) => t.id === payload.threadId);
      if (existing) {
        replaceThread({ ...existing, interactionMode: payload.interactionMode });
      }
      for (const pane of getAllPanes().values()) {
        if (pane.threadId !== payload.threadId) continue;
        if (pane.thread) {
          pane.replaceThread({ ...pane.thread, interactionMode: payload.interactionMode });
        }
      }
      if (payload.needsReconnect) {
        addToast(
          'warning',
          `Mode set to ${payload.interactionMode}. Reconnect the session to apply.`,
        );
      }
    },
  );

  return () => {
    cancelEvent();
    cancelMeta();
    cancelError();
    cancelDesignArtifact();
    cancelDesignOptions();
    cancelDesignChosen();
    cancelModeChanged();
    cancelRuntimeModeChanged();
  };
}
