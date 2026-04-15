import { EventsOn } from '../../../wailsjs/runtime/runtime';
import type { ProviderEvent, ApprovalRequest, TokenUsage } from '../types/events';
import type { PayloadMeta } from '../types/models';
import type { ThreadPane } from './thread.svelte';
import { getAllPanes } from './panes.svelte';

/**
 * Route a provider event to the correct pane mutation.
 * Called once per pane that matches the event's threadId.
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
      pane.completeToolCall(evt.itemId ?? '', {
        id: evt.itemId ?? '',
        threadId: evt.threadId,
        turnIndex: 0,
        itemIndex: 0,
        kind: evt.itemType ?? 'tool_result',
        role: 'assistant',
        summary: evt.content ?? '',
        createdAt: Date.now(),
      });
      break;

    case 'turn_start':
      pane.setSessionStatus('running');
      break;

    case 'turn_complete':
      pane.setSessionStatus('ready');
      break;

    case 'approval_request':
      if (evt.meta) {
        pane.addApproval(evt.meta as ApprovalRequest);
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
      break;

    case 'init':
      pane.setSessionStatus('connected');
      break;

    case 'background_start':
      pane.addBackgroundTask(evt.itemId ?? '', evt.meta);
      break;

    case 'background_complete':
      pane.completeBackgroundTask(evt.itemId ?? '');
      break;
  }
}

export function setupEventListeners(): void {
  EventsOn('provider:event', (evt: ProviderEvent) => {
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === evt.threadId) {
        routeEventToPane(pane, evt);
      }
    }
  });

  EventsOn('provider:meta', (meta: PayloadMeta) => {
    for (const pane of getAllPanes().values()) {
      pane.addPayloadMeta(meta);
    }
  });

  EventsOn('provider:error', (evt: ProviderEvent) => {
    console.error('Provider error event:', evt.content);
  });
}
