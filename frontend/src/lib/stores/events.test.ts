import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { setupEventListeners } from './events';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import { getAllPanes } from './panes.svelte';
import type { Thread } from '../types/models';
import type { ProviderEvent, EventKind } from '../types/events';
import { emitWailsEvent, wailsListenerCount } from '../../test/mocks/wailsio-runtime';
import { setBindingMock, getBindingMock } from '../../test/mocks/bindings-app';

function thread(id: string): Thread {
  return {
    id,
    title: `Thread ${id}`,
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    interactionMode: 'default',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

function event(kind: EventKind, overrides: Partial<ProviderEvent> = {}): ProviderEvent {
  return {
    kind,
    threadId: 'thread-1',
    timestamp: '2024-01-01T00:00:00Z',
    ...overrides,
  };
}

/**
 * Build a new pane, attach it as the 'main' pane so the event router can find it,
 * and return the pane. The router iterates `getAllPanes()` — we clear between
 * tests to keep isolation.
 */
async function installPane(t: Thread): Promise<ThreadPane> {
  const pane = createThreadPane();
  // Inject into the pane registry by mutating the exported map.
  const all = getAllPanes();
  all.set('main', pane);
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  await pane.switchThread(t);
  return pane;
}

describe('events router', () => {
  let cleanup: () => void;

  beforeEach(() => {
    // Clear any panes left over.
    getAllPanes().clear();
    cleanup = setupEventListeners();
  });

  afterEach(() => {
    cleanup();
    getAllPanes().clear();
  });

  describe('listener lifecycle', () => {
    it('registers listeners for the three channels', () => {
      expect(wailsListenerCount('provider:event')).toBe(1);
      expect(wailsListenerCount('provider:meta')).toBe(1);
      expect(wailsListenerCount('provider:error')).toBe(1);
    });

    it('cleanup unregisters all listeners', () => {
      cleanup();
      expect(wailsListenerCount('provider:event')).toBe(0);
      expect(wailsListenerCount('provider:meta')).toBe(0);
      expect(wailsListenerCount('provider:error')).toBe(0);
      // Re-install so afterEach cleanup doesn't double-unregister.
      cleanup = setupEventListeners();
    });

    it('routes events only to panes whose threadId matches', async () => {
      const paneA = await installPane(thread('thread-A'));
      // Second pane attached under a different key.
      const paneB = createThreadPane();
      getAllPanes().set('secondary', paneB);
      setBindingMock('ListItems', async () => []);
      setBindingMock('ListPayloadMetas', async () => []);
      await paneB.switchThread(thread('thread-B'));

      emitWailsEvent('provider:event', event('text_delta', { threadId: 'thread-A', content: 'for A' }));
      expect(paneA.streamingContent).toBe('for A');
      expect(paneB.streamingContent).toBe('');
    });

    it('ignores events whose threadId matches no pane', async () => {
      await installPane(thread('thread-A'));
      // Should not throw.
      emitWailsEvent('provider:event', event('text_delta', { threadId: 'ghost', content: 'x' }));
    });
  });

  describe('text_delta', () => {
    it('appends content', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('text_delta', { content: 'Hello' }));
      emitWailsEvent('provider:event', event('text_delta', { content: ', world' }));
      expect(pane.streamingContent).toBe('Hello, world');
    });

    it('treats missing content as empty string', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('text_delta'));
      expect(pane.streamingContent).toBe('');
    });
  });

  describe('tool_start / tool_progress / tool_complete', () => {
    it('tool_start adds entry, tool_complete removes it', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('tool_start', {
        itemId: 'tool-42',
        meta: { toolName: 'bash' },
      }));
      expect(pane.activeToolCalls.size).toBe(1);
      expect(pane.activeToolCalls.get('tool-42')).toEqual({ toolName: 'bash' });

      emitWailsEvent('provider:event', event('tool_complete', { itemId: 'tool-42' }));
      expect(pane.activeToolCalls.size).toBe(0);
    });

    it('tool_progress updates data for a known tool call', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('tool_start', {
        itemId: 'tool-1',
        meta: { toolName: 'bash' },
      }));
      emitWailsEvent('provider:event', event('tool_progress', {
        itemId: 'tool-1',
        meta: { current: 3, total: 10, message: 'step 3' },
      }));
      expect(pane.activeToolCalls.get('tool-1')).toEqual({ current: 3, total: 10, message: 'step 3' });
    });

    it('tool_progress without itemId or meta is ignored', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('tool_start', {
        itemId: 'tool-1',
        meta: { toolName: 'bash' },
      }));
      emitWailsEvent('provider:event', event('tool_progress', { itemId: 'tool-1' }));
      // Unchanged.
      expect(pane.activeToolCalls.get('tool-1')).toEqual({ toolName: 'bash' });
    });
  });

  describe('turn_start / turn_complete', () => {
    it('turn_start sets sessionStatus to running', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('turn_start'));
      expect(pane.sessionStatus).toBe('running');
    });

    it('turn_complete sets sessionStatus to ready and reloads items', async () => {
      const pane = await installPane(thread('thread-1'));
      pane.appendTextDelta('partial');
      setBindingMock('ListItems', async () => []);

      emitWailsEvent('provider:event', event('turn_complete'));
      expect(pane.sessionStatus).toBe('ready');
      // finalizeTurn clears streaming synchronously, reload is async.
      expect(pane.streamingContent).toBe('');
    });
  });

  describe('approval_request / approval_resolved', () => {
    it('approval_request queues the approval when not session-approved', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('approval_request', {
        meta: {
          requestId: 'req-1',
          threadId: 'thread-1',
          toolName: 'bash',
          description: '',
          input: null,
          title: 'approve bash',
        },
      }));
      expect(pane.pendingApprovals).toHaveLength(1);
      expect(pane.pendingApprovals[0].requestId).toBe('req-1');
    });

    it('approval_request auto-resolves when tool is session-approved', async () => {
      const pane = await installPane(thread('thread-1'));
      pane.addSessionApprovedTool('bash');
      const respond = setBindingMock('RespondToApproval', async () => {});

      emitWailsEvent('provider:event', event('approval_request', {
        meta: {
          requestId: 'req-auto',
          threadId: 'thread-1',
          toolName: 'bash',
          description: '',
          input: null,
          title: 'approve bash',
        },
      }));

      expect(respond).toHaveBeenCalledTimes(1);
      expect(pane.pendingApprovals).toHaveLength(0);
    });

    it('approval_request falls back to queueing if auto-approve RPC fails', async () => {
      const pane = await installPane(thread('thread-1'));
      pane.addSessionApprovedTool('bash');
      setBindingMock('RespondToApproval', async () => { throw new Error('rpc fail'); });

      emitWailsEvent('provider:event', event('approval_request', {
        meta: {
          requestId: 'req-fallback',
          threadId: 'thread-1',
          toolName: 'bash',
          description: '',
          input: null,
          title: 'approve bash',
        },
      }));
      // Promise rejection is async.
      await Promise.resolve();
      await Promise.resolve();
      expect(pane.pendingApprovals).toHaveLength(1);
    });

    it('approval_resolved removes a pending approval by itemId', async () => {
      const pane = await installPane(thread('thread-1'));
      pane.addApproval({
        requestId: 'req-X',
        threadId: 'thread-1',
        toolName: 'edit',
        description: '',
        input: null,
        title: '',
      });
      emitWailsEvent('provider:event', event('approval_resolved', { itemId: 'req-X' }));
      expect(pane.pendingApprovals).toHaveLength(0);
    });

    it('approval_request with no meta is ignored', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('approval_request'));
      expect(pane.pendingApprovals).toHaveLength(0);
    });
  });

  describe('session_status / init / error', () => {
    it('session_status updates the pane status', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('session_status', { content: 'reconnecting' }));
      expect(pane.sessionStatus).toBe('reconnecting');
    });

    it('session_status with no content falls back to unknown', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('session_status'));
      expect(pane.sessionStatus).toBe('unknown');
    });

    it('init sets sessionStatus to connected', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('init'));
      expect(pane.sessionStatus).toBe('connected');
    });

    it('error sets status and error message', async () => {
      const pane = await installPane(thread('thread-1'));
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
      emitWailsEvent('provider:event', event('error', { content: 'boom' }));
      expect(pane.sessionStatus).toBe('error');
      expect(pane.error).toBe('boom');
      consoleErr.mockRestore();
    });

    it('error without content falls back to generic message', async () => {
      const pane = await installPane(thread('thread-1'));
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
      emitWailsEvent('provider:event', event('error'));
      expect(pane.error).toBe('Unknown provider error');
      consoleErr.mockRestore();
    });
  });

  describe('token_usage', () => {
    it('updates pane tokenUsage from meta', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('token_usage', {
        meta: { inputTokens: 100, outputTokens: 50, totalCostUsd: 0.001 },
      }));
      expect(pane.tokenUsage).toEqual({ inputTokens: 100, outputTokens: 50, totalCostUsd: 0.001 });
    });

    it('token_usage without meta is ignored', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('token_usage'));
      expect(pane.tokenUsage).toBeNull();
    });
  });

  describe('background_start / background_delta / background_complete', () => {
    it('background_start adds and background_complete removes', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('background_start', {
        itemId: 'bg-1',
        meta: { name: 'build' },
      }));
      expect(pane.backgroundTasks.size).toBe(1);

      emitWailsEvent('provider:event', event('background_complete', { itemId: 'bg-1' }));
      expect(pane.backgroundTasks.size).toBe(0);
    });

    it('background_delta is inert (accumulated server-side)', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('background_start', {
        itemId: 'bg-1',
        meta: { name: 'build' },
      }));
      emitWailsEvent('provider:event', event('background_delta', {
        itemId: 'bg-1',
        content: 'log line',
      }));
      // No frontend state mutation for background deltas.
      expect(pane.backgroundTasks.get('bg-1')).toEqual({ name: 'build' });
    });
  });

  describe('heavy-payload events are inert on the router', () => {
    // diff / command_output / thinking events don't mutate pane state directly.
    // They're persisted by the backend and arrive as items + payload metas.
    it.each<EventKind>(['diff', 'command_output', 'thinking'])(
      '%s leaves pane state untouched',
      async (kind) => {
        const pane = await installPane(thread('thread-1'));
        const snapshot = {
          items: pane.items.length,
          metas: pane.payloadMetas.size,
          stream: pane.streamingContent,
        };
        emitWailsEvent('provider:event', event(kind, { itemId: 'x', content: 'ignored' }));
        expect(pane.items.length).toBe(snapshot.items);
        expect(pane.payloadMetas.size).toBe(snapshot.metas);
        expect(pane.streamingContent).toBe(snapshot.stream);
      },
    );
  });

  describe('compact_boundary', () => {
    it('updates pane contextWindow from meta', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('compact_boundary', {
        meta: { usedTokens: 50, maxTokens: 8000, usedPercentage: 0.62 },
      }));
      expect(pane.contextWindow).toEqual({ usedTokens: 50, maxTokens: 8000, usedPercentage: 0.62 });
    });
  });

  describe('rate_limits', () => {
    it('populates limits from meta', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('rate_limits', {
        meta: {
          limits: [
            { limitId: 'L1', limitName: '1h', usedPercent: 0.2, windowMins: 60, resetsAt: 100 },
            { limitId: 'L2', limitName: '24h', usedPercent: 0.4, windowMins: 1440, resetsAt: 200 },
          ],
        },
      }));
      expect(pane.rateLimits).toHaveLength(2);
      expect(pane.rateLimits[0].limitId).toBe('L1');
    });

    it('handles meta with missing limits array gracefully', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('rate_limits', { meta: {} }));
      expect(pane.rateLimits).toEqual([]);
    });
  });

  describe('model_rerouted', () => {
    it('updates pane.thread.model and emits a toast', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('model_rerouted', {
        meta: { newModel: 'claude-opus-4-7' },
      }));
      expect(pane.thread?.model).toBe('claude-opus-4-7');
    });
  });

  describe('thread_renamed', () => {
    it('updates pane.thread.title', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', event('thread_renamed', {
        meta: { newTitle: 'Updated title' },
      }));
      expect(pane.thread?.title).toBe('Updated title');
    });
  });

  describe('unknown event kind', () => {
    it('is silently ignored (no throw, no state change)', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', {
        kind: 'nonexistent_kind' as EventKind,
        threadId: 'thread-1',
        timestamp: '0',
      });
      expect(pane.items).toEqual([]);
      expect(pane.streamingContent).toBe('');
    });
  });

  describe('provider:meta channel', () => {
    it('adds payload meta to pane owning the thread', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:meta', {
        id: 'p1',
        threadId: 'thread-1',
        kind: 'diff',
        meta: '{}',
        createdAt: 0,
      });
      expect(pane.payloadMetas.size).toBe(1);
      expect(pane.payloadMetas.get('p1')?.kind).toBe('diff');
    });

    it('skips panes whose threadId does not match', async () => {
      const paneA = await installPane(thread('thread-A'));
      const paneB = createThreadPane();
      getAllPanes().set('b', paneB);
      setBindingMock('ListItems', async () => []);
      setBindingMock('ListPayloadMetas', async () => []);
      await paneB.switchThread(thread('thread-B'));

      emitWailsEvent('provider:meta', {
        id: 'p1',
        threadId: 'thread-A',
        kind: 'diff',
        meta: '{}',
        createdAt: 0,
      });

      expect(paneA.payloadMetas.size).toBe(1);
      expect(paneB.payloadMetas.size).toBe(0);
    });

    it('broadcasts to all panes when threadId is absent (legacy)', async () => {
      const paneA = await installPane(thread('thread-A'));
      const paneB = createThreadPane();
      getAllPanes().set('b', paneB);
      setBindingMock('ListItems', async () => []);
      setBindingMock('ListPayloadMetas', async () => []);
      await paneB.switchThread(thread('thread-B'));

      emitWailsEvent('provider:meta', {
        id: 'p1',
        kind: 'diff',
        meta: '{}',
        createdAt: 0,
      });

      expect(paneA.payloadMetas.size).toBe(1);
      expect(paneB.payloadMetas.size).toBe(1);
    });
  });

  describe('provider:error channel', () => {
    it('sets error on matching pane and logs', async () => {
      const pane = await installPane(thread('thread-1'));
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

      emitWailsEvent('provider:error', event('error', { content: 'stdio died' }));

      expect(pane.error).toBe('stdio died');
      expect(consoleErr).toHaveBeenCalled();
      consoleErr.mockRestore();
    });

    it('falls back to generic error message when content missing', async () => {
      const pane = await installPane(thread('thread-1'));
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

      emitWailsEvent('provider:error', event('error'));

      expect(pane.error).toBe('Unknown provider error');
      consoleErr.mockRestore();
    });

    it('does not touch panes whose threadId differs', async () => {
      const paneA = await installPane(thread('thread-A'));
      const paneB = createThreadPane();
      getAllPanes().set('b', paneB);
      setBindingMock('ListItems', async () => []);
      setBindingMock('ListPayloadMetas', async () => []);
      await paneB.switchThread(thread('thread-B'));
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

      emitWailsEvent('provider:error', event('error', { threadId: 'thread-A', content: 'A died' }));

      expect(paneA.error).toBe('A died');
      expect(paneB.error).toBeNull();
      consoleErr.mockRestore();
    });
  });

  describe('RespondToApproval auto-resolution payload', () => {
    it('sends allow decision with the approval requestId', async () => {
      const pane = await installPane(thread('thread-1'));
      pane.addSessionApprovedTool('edit');
      setBindingMock('RespondToApproval', async () => {});

      emitWailsEvent('provider:event', event('approval_request', {
        meta: {
          requestId: 'req-auto-args',
          threadId: 'thread-1',
          toolName: 'edit',
          description: '',
          input: null,
          title: 'approve edit',
        },
      }));

      const mock = getBindingMock('RespondToApproval');
      expect(mock).toBeDefined();
      expect(mock!.mock.calls).toHaveLength(1);
      const [calledThreadId, response] = mock!.mock.calls[0];
      expect(calledThreadId).toBe('thread-1');
      expect((response as { requestId: string; decision: string }).requestId).toBe('req-auto-args');
      expect((response as { requestId: string; decision: string }).decision).toBe('allow');
    });
  });

  describe('design:artifact channel', () => {
    it('appends an artifact to the pane whose threadId matches', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('design:artifact', {
        id: 'a-1',
        threadId: 'thread-1',
        title: 'Landing page',
        description: '',
        kind: 'render',
        htmlPath: '/tmp/a-1.html',
        createdAt: 100,
      });
      expect(pane.designArtifacts).toHaveLength(1);
      expect(pane.designArtifacts[0].id).toBe('a-1');
    });

    it('ignores artifacts for other threads', async () => {
      const paneA = await installPane(thread('thread-A'));
      const paneB = createThreadPane();
      getAllPanes().set('b', paneB);
      setBindingMock('ListItems', async () => []);
      setBindingMock('ListPayloadMetas', async () => []);
      await paneB.switchThread(thread('thread-B'));

      emitWailsEvent('design:artifact', {
        id: 'a-1',
        threadId: 'thread-A',
        title: 'A-specific',
        description: '',
        kind: 'render',
        htmlPath: '',
        createdAt: 0,
      });

      expect(paneA.designArtifacts).toHaveLength(1);
      expect(paneB.designArtifacts).toHaveLength(0);
    });

    it('de-duplicates repeated artifact IDs', async () => {
      const pane = await installPane(thread('thread-1'));
      const artifact = {
        id: 'dup',
        threadId: 'thread-1',
        title: 'Card',
        description: '',
        kind: 'render',
        htmlPath: '',
        createdAt: 0,
      };
      emitWailsEvent('design:artifact', artifact);
      emitWailsEvent('design:artifact', artifact);
      expect(pane.designArtifacts).toHaveLength(1);
    });

    it('ignores payload with no threadId', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('design:artifact', {
        id: 'a', title: 't', description: '', kind: 'render', htmlPath: '', createdAt: 0,
      });
      expect(pane.designArtifacts).toHaveLength(0);
    });
  });

  describe('design:options channel', () => {
    it('sets pendingDesignOptions on the matching pane', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('design:options', {
        requestId: 'req-42',
        threadId: 'thread-1',
        prompt: 'Pick a hero layout',
        options: [
          { id: 'opt-1', title: 'Bold', description: '', artifactId: 'art-1' },
          { id: 'opt-2', title: 'Minimal', description: '', artifactId: 'art-2' },
        ],
      });
      expect(pane.pendingDesignOptions?.requestId).toBe('req-42');
      expect(pane.pendingDesignOptions?.options).toHaveLength(2);
    });

    it('scopes to the matching pane only', async () => {
      const paneA = await installPane(thread('thread-A'));
      const paneB = createThreadPane();
      getAllPanes().set('b', paneB);
      setBindingMock('ListItems', async () => []);
      setBindingMock('ListPayloadMetas', async () => []);
      await paneB.switchThread(thread('thread-B'));

      emitWailsEvent('design:options', {
        requestId: 'r1',
        threadId: 'thread-B',
        prompt: '',
        options: [{ id: 'o1', title: 'x', description: '', artifactId: 'a' }],
      });

      expect(paneA.pendingDesignOptions).toBeNull();
      expect(paneB.pendingDesignOptions?.requestId).toBe('r1');
    });
  });

  describe('design:chosen channel', () => {
    it('clears pendingDesignOptions when the requestId matches', async () => {
      const pane = await installPane(thread('thread-1'));
      pane.setDesignOptions({
        requestId: 'req-X',
        threadId: 'thread-1',
        prompt: '',
        options: [{ id: 'o1', title: 't', description: '', artifactId: 'a' }],
      });
      emitWailsEvent('design:chosen', {
        threadId: 'thread-1',
        requestId: 'req-X',
        optionId: 'o1',
        title: 't',
      });
      expect(pane.pendingDesignOptions).toBeNull();
    });

    it('does not clear when a stale chosen event references a different requestId', async () => {
      const pane = await installPane(thread('thread-1'));
      pane.setDesignOptions({
        requestId: 'req-CURRENT',
        threadId: 'thread-1',
        prompt: '',
        options: [{ id: 'o1', title: 't', description: '', artifactId: 'a' }],
      });
      emitWailsEvent('design:chosen', {
        threadId: 'thread-1',
        requestId: 'req-STALE',
        optionId: 'o1',
        title: 't',
      });
      expect(pane.pendingDesignOptions?.requestId).toBe('req-CURRENT');
    });

    it('ignores events for other threads', async () => {
      const paneA = await installPane(thread('thread-A'));
      paneA.setDesignOptions({
        requestId: 'req-A',
        threadId: 'thread-A',
        prompt: '',
        options: [{ id: 'o1', title: 't', description: '', artifactId: 'a' }],
      });
      emitWailsEvent('design:chosen', {
        threadId: 'thread-B',
        requestId: 'req-A',
        optionId: 'o1',
        title: 't',
      });
      expect(paneA.pendingDesignOptions?.requestId).toBe('req-A');
    });
  });

  describe('design listener lifecycle', () => {
    it('registers listeners for the three design channels', () => {
      expect(wailsListenerCount('design:artifact')).toBe(1);
      expect(wailsListenerCount('design:options')).toBe(1);
      expect(wailsListenerCount('design:chosen')).toBe(1);
    });

    it('cleanup unregisters design listeners too', () => {
      cleanup();
      expect(wailsListenerCount('design:artifact')).toBe(0);
      expect(wailsListenerCount('design:options')).toBe(0);
      expect(wailsListenerCount('design:chosen')).toBe(0);
      cleanup = setupEventListeners();
    });
  });

  // Task B (seq envelope): the wailsEventOn helper unwraps SeqEnvelope
  // payloads and logs a console.warn whenever the seq jumps past 1. The
  // frontend is purely observability scaffolding — it never blocks or
  // buffers, so every test here asserts both the delivered payload and
  // the console side-effect.
  describe('seq envelope gap detection', () => {
    it('routes enveloped payloads to handlers with the inner data', async () => {
      const pane = await installPane(thread('thread-1'));
      emitWailsEvent('provider:event', {
        seq: 1,
        data: event('text_delta', { content: 'hi' }),
      });
      expect(pane.streamingContent).toBe('hi');
    });

    it('warns when a seq gap appears, logging the missing range', async () => {
      await installPane(thread('thread-1'));
      const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => {});

      emitWailsEvent('provider:event', { seq: 1, data: event('text_delta', { content: 'a' }) });
      emitWailsEvent('provider:event', { seq: 2, data: event('text_delta', { content: 'b' }) });
      emitWailsEvent('provider:event', { seq: 5, data: event('text_delta', { content: 'c' }) });

      // Gap: ids 3 and 4 missing.
      expect(consoleWarn).toHaveBeenCalledTimes(1);
      const [message] = consoleWarn.mock.calls[0];
      expect(message).toContain('provider:event');
      expect(message).toContain('missing 2');
      expect(message).toContain('3..4');
      consoleWarn.mockRestore();
    });

    it('stays silent on the first event of a channel (nothing to compare against)', async () => {
      await installPane(thread('thread-1'));
      const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => {});

      emitWailsEvent('provider:event', { seq: 42, data: event('text_delta', { content: 'a' }) });

      expect(consoleWarn).not.toHaveBeenCalled();
      consoleWarn.mockRestore();
    });

    it('stays silent on consecutive monotonic seqs', async () => {
      await installPane(thread('thread-1'));
      const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => {});

      for (let i = 1; i <= 5; i++) {
        emitWailsEvent('provider:event', { seq: i, data: event('text_delta', { content: 'x' }) });
      }

      expect(consoleWarn).not.toHaveBeenCalled();
      consoleWarn.mockRestore();
    });

    it('does not warn or break on raw (pre-envelope) payloads', async () => {
      const pane = await installPane(thread('thread-1'));
      const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => {});

      // Raw event, no envelope — the existing tests in this file drive
      // this shape, and production emissions during rollout may do the
      // same. The helper must pass through the payload untouched.
      emitWailsEvent('provider:event', event('text_delta', { content: 'raw' }));

      expect(pane.streamingContent).toBe('raw');
      expect(consoleWarn).not.toHaveBeenCalled();
      consoleWarn.mockRestore();
    });

    it('does not roll the seq pointer backward on a stale re-delivery', async () => {
      await installPane(thread('thread-1'));
      const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => {});

      // Deliver 5 in order, then a stale 3. The pointer must stay at 5
      // so a subsequent 6 is treated as consecutive (no warn), not as
      // "2..5 missing".
      for (let i = 1; i <= 5; i++) {
        emitWailsEvent('provider:event', { seq: i, data: event('text_delta') });
      }
      emitWailsEvent('provider:event', { seq: 3, data: event('text_delta') });
      emitWailsEvent('provider:event', { seq: 6, data: event('text_delta') });

      expect(consoleWarn).not.toHaveBeenCalled();
      consoleWarn.mockRestore();
    });

    it('tracks seq independently per channel', async () => {
      await installPane(thread('thread-1'));
      const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => {});

      emitWailsEvent('provider:event', { seq: 10, data: event('text_delta') });
      // First provider:meta: no prior baseline on this channel, must
      // not warn even though its seq is far below provider:event's.
      emitWailsEvent('provider:meta', {
        seq: 1,
        data: { id: 'p1', threadId: 'thread-1', kind: 'diff', meta: '{}', createdAt: 0 },
      });

      expect(consoleWarn).not.toHaveBeenCalled();
      consoleWarn.mockRestore();
    });
  });
});
