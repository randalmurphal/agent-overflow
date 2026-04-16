import { Events } from '@wailsio/runtime';
import type { ProviderEvent, ApprovalRequest, ContextWindow, RateLimitEntry, TokenUsage, ToolProgressMeta } from '../types/events';
import type { PayloadMeta } from '../types/models';
import type { ThreadPane } from './thread.svelte';
import { getAllPanes } from './panes.svelte';
import { addToast } from './toast.svelte';
import { updateThreadTitle } from './threads.svelte';
import { RespondToApproval, ApprovalResponse } from './bindings';

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
        pane.addToolCall(evt.itemId, evt.meta as ToolProgressMeta);
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
        addToast('info', `Model rerouted to ${data.newModel}`);
      }
      break;

    case 'thread_renamed':
      if (evt.meta) {
        const data = evt.meta as { newTitle: string };
        updateThreadTitle(evt.threadId, data.newTitle);
      }
      break;
  }
}

/**
 * Set up Wails event listeners for provider events.
 * Returns a cleanup function that removes all listeners.
 */
export function setupEventListeners(): () => void {
  const cancelEvent = Events.On('provider:event', (ev) => {
    const evt = ev.data as ProviderEvent;
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === evt.threadId) {
        routeEventToPane(pane, evt);
      }
    }
  });

  const cancelMeta = Events.On('provider:meta', (ev) => {
    const meta = ev.data as PayloadMeta;
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

  const cancelError = Events.On('provider:error', (ev) => {
    const evt = ev.data as ProviderEvent;
    console.error('Provider error event:', evt.content);
    addToast('error', evt.content ?? 'Provider error');
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === evt.threadId) {
        pane.setError(evt.content ?? 'Unknown provider error');
      }
    }
  });

  return () => {
    cancelEvent();
    cancelMeta();
    cancelError();
  };
}
