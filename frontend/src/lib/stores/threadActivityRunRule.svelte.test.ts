// The pane groups a thread's agent messages by the membership rule its
// owning backend advertises (`activity-runs.agent-message-members.v1`), and
// asks for that rule exactly when it is advertised. Against each kind of
// backend it settles on that backend's runs with a bounded number of pages
// and members calls and a held window it can describe; a hello that changes
// the rule regroups the pane through the reconnect refresh; and a
// disagreement no reload can settle ends in one visible failure.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { activityRunProse, activityRunRow } from '../../test/helpers/activityRuns';
import { serveClassifiedHistory, type AgentMessageRuleOf } from '../../test/helpers/activityRunBackend';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { makeThread } from '../../test/helpers/chat';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { createTimelineRowProjection } from '../components/chat/timelineRowProjection.svelte';
import { noteThread } from '../transport/entityIndex';
import { setCarriedSessionScopes } from '../transport/scopes';
import type { TransportHello } from '../transport/wsClient';
import type { Item } from '../types/models';
import type { TimelineNode } from '../utils/subagentGrouping';
import { AGENT_MESSAGE_RUN_MEMBERS_CAPABILITY } from './activityRunRule';
import { installComputerHydration } from './computerHydration';
import { registerPaneForTest, resetPanesForTest } from './panes.svelte';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import { getToasts } from './toast.svelte';
import { __setTransportHelloForTest } from './transportStatus.svelte';

function agentMessage(id: string, index: number): Item {
  return activityRunRow(id, index, {
    kind: 'user_text',
    role: 'user',
    toolName: '',
    summary: 'worker reply',
    meta: JSON.stringify({ agent_message: { sender: 'worker' } }),
  });
}

function hello(backendId: string, members: boolean): TransportHello {
  return {
    protocolVersion: 1,
    capabilities: ['timeline.scopes.v1', ...(members ? [AGENT_MESSAGE_RUN_MEMBERS_CAPABILITY] : [])],
    backendId, backendName: '', serverTimeMs: 0, clockSkewMs: 0,
    bundleId: '', bundleVersion: '', minShellBuild: 0,
  } as TransportHello;
}

interface History {
  rows: () => Item[];
  members: string[];
  absorbed: string[];
}

const HISTORIES: Record<string, History> = {
  'agent message, tool': {
    rows: () => [agentMessage('m', 0), activityRunRow('t', 1)],
    members: ['run[m,t]'],
    absorbed: ['m', 'run[t]'],
  },
  'prose, agent message, tool': {
    rows: () => [activityRunProse('p', 0), agentMessage('m', 1), activityRunRow('t', 2)],
    members: ['p', 'run[m,t]'],
    absorbed: ['p', 'm', 'run[t]'],
  },
  'tool, prose, agent message': {
    rows: () => [activityRunRow('t', 0), activityRunProse('p', 1), agentMessage('m', 2)],
    members: ['run[t]', 'p', 'run[m]'],
    absorbed: ['run[t]', 'p', 'm'],
  },
};

/** The rule a backend that honors the request field reads off each call. */
const requestedRule: AgentMessageRuleOf = request =>
  Boolean(request.agentMessageMembers ?? (request.shape as { agentMessageMembers?: boolean } | undefined)?.agentMessageMembers);

/** What each call asked for: the page options' or members shape's field. */
function asked(backend: ReturnType<typeof serveClassifiedHistory>): { pages: unknown[]; members: unknown[] } {
  return {
    pages: backend.pages.mock.calls.map(call => (call[3] as { agentMessageMembers?: boolean }).agentMessageMembers),
    members: backend.members.mock.calls.map(call =>
      (call[1] as { shape?: { agentMessageMembers?: boolean } }).shape?.agentMessageMembers),
  };
}

function shapeOf(nodes: readonly TimelineNode[]): string[] {
  return nodes.map(node => {
    if (node.kind === 'leaf') return node.item.id;
    if (node.kind === 'activity_run') return `run[${shapeOf(node.children).join(',')}]`;
    return node.kind;
  });
}

/** The pane's timeline as a mounted timeline projects it. */
function mountProjection(pane: ThreadPane): { readonly shape: string[]; dispose(): void } {
  let nodes: TimelineNode[] = [];
  const dispose = $effect.root(() => {
    const projection = createTimelineRowProjection({ getPane: () => pane });
    $effect(() => { nodes = projection.revealedNodes; });
  });
  return { get shape() { return shapeOf(nodes); }, dispose };
}

let cleanup: (() => void)[] = [];

beforeEach(() => {
  installThreadPaneTestEnv();
  vi.useFakeTimers();
});

afterEach(() => {
  for (const step of cleanup.reverse()) step();
  cleanup = [];
  vi.useRealTimers();
  __setTransportHelloForTest(null);
  resetPanesForTest();
  resetStagedBackends();
});

async function openPane(): Promise<{ pane: ThreadPane; projection: { readonly shape: string[] } }> {
  const pane = createThreadPane({ paneId: 'main' });
  registerPaneForTest('main', pane);
  await pane.switchThread(makeThread({ id: 't' }));
  const projection = mountProjection(pane);
  cleanup.push(() => { projection.dispose(); pane.clear(); });
  return { pane, projection };
}

function settled(pane: ThreadPane, backend: ReturnType<typeof serveClassifiedHistory>, pages: number) {
  expect(backend.pages.mock.calls.length).toBeLessThanOrEqual(pages);
  expect(backend.members.mock.calls.length).toBeLessThanOrEqual(1);
  expect(pane.activityRuns.heldRunFold()).not.toBeNull();
}

describe('a backend without the capability', () => {
  it.each(Object.keys(HISTORIES))('groups %s by the absorbed rule, asks for nothing, and settles', async (name) => {
    __setTransportHelloForTest(hello('', false));
    const history = HISTORIES[name].rows();
    const backend = serveClassifiedHistory(() => history, false);
    const { pane, projection } = await openPane();
    await vi.advanceTimersByTimeAsync(5000);
    settled(pane, backend, 1);
    expect(projection.shape).toEqual(HISTORIES[name].absorbed);
    expect(asked(backend).pages.every(field => field === undefined)).toBe(true);
    expect(asked(backend).members.every(field => field === undefined)).toBe(true);
  });
});

describe('a backend with the capability', () => {
  it.each(Object.keys(HISTORIES))('groups %s by the member-on-sight rule it asked for, and settles', async (name) => {
    __setTransportHelloForTest(hello('', true));
    const history = HISTORIES[name].rows();
    const backend = serveClassifiedHistory(() => history, requestedRule);
    const { pane, projection } = await openPane();
    await vi.advanceTimersByTimeAsync(5000);
    settled(pane, backend, 1);
    expect(projection.shape).toEqual(HISTORIES[name].members);
    expect(asked(backend).pages).toEqual(backend.pages.mock.calls.map(() => true));
    expect(asked(backend).members.every(field => field === true)).toBe(true);
  });
});

describe('a live agent message', () => {
  it.each([
    [true, ['run[t0]', 'p1', 'run[m2,t3]']],
    [false, ['run[t0]', 'p1', 'm2', 'run[t3]']],
  ] as const)('groups by the backend rule live and after a reload (members %s)', async (members, shape) => {
    __setTransportHelloForTest(hello('', members));
    const history = [activityRunRow('t0', 0), activityRunProse('p1', 1)];
    const backend = serveClassifiedHistory(() => history, requestedRule);
    const { pane, projection } = await openPane();
    const pushed = [agentMessage('m2', 2), activityRunRow('t3', 3)];
    history.push(...pushed);
    pane.applyProviderItemUpserts(pushed);
    await vi.advanceTimersByTimeAsync(5000);
    expect(projection.shape).toEqual(shape);
    await pane.refreshFromBackend(true);
    await vi.advanceTimersByTimeAsync(5000);
    expect(projection.shape).toEqual(shape);
    settled(pane, backend, 2);
  });
});

describe('a hello that changes the rule', () => {
  it.each(Object.keys(HISTORIES))('regroups %s through the reconnect refresh, both ways', async (name) => {
    const gpu = stageBackend({ id: 'gpu', hello: hello('gpu', false) });
    setCarriedSessionScopes('gpu', ['threads:read']);
    noteThread('t', 'gpu');
    setBindingMock('ListThreadLiveActivity', async () => []);
    setBindingMock('GetRateLimitsSnapshots', async () => []);
    setBindingMock('ListUnsettledFlushes', async () => []);
    const history = HISTORIES[name].rows();
    const backend = serveClassifiedHistory(() => history, requestedRule);
    const { pane, projection } = await openPane();
    const stop = installComputerHydration();
    cleanup.push(stop);
    await vi.advanceTimersByTimeAsync(5000);
    expect(projection.shape).toEqual(HISTORIES[name].absorbed);

    for (const members of [true, false]) {
      backend.pages.mockClear();
      backend.members.mockClear();
      gpu.setHello(hello('gpu', members));
      await vi.advanceTimersByTimeAsync(5000);
      expect(projection.shape).toEqual(members ? HISTORIES[name].members : HISTORIES[name].absorbed);
      settled(pane, backend, 1);
      expect(asked(backend).pages).toEqual([members ? true : undefined]);
    }
    expect(getToasts()).toEqual([]);
  });
});

describe('a disagreement no reload can settle', () => {
  // Whether the pane holds a run the backend's stubs describe differently.
  // In the third history the message's run has no stub to disagree with.
  const disagrees: Record<string, boolean> = {
    'agent message, tool': true,
    'prose, agent message, tool': true,
    'tool, prose, agent message': false,
  };

  it.each(Object.keys(HISTORIES))('reports %s once and stops asking', async (name) => {
    // The hello claims the rule; the backend groups by the other one.
    __setTransportHelloForTest(hello('', true));
    const history = HISTORIES[name].rows();
    const backend = serveClassifiedHistory(() => history, false);
    const shown = new Map<string, string>();
    const watch = async (ms: number) => {
      for (let elapsed = 0; elapsed < ms; elapsed += 100) {
        await vi.advanceTimersByTimeAsync(100);
        for (const toast of getToasts()) shown.set(toast.id, toast.message);
      }
    };
    const { pane, projection } = await openPane();
    await watch(5000);
    const pages = backend.pages.mock.calls.length;
    const members = backend.members.mock.calls.length;
    // The first page, and one reload for the refusal.
    expect(pages).toBe(disagrees[name] ? 2 : 1);
    expect(members).toBe(disagrees[name] ? 2 : 0);
    expect([...shown.values()]).toEqual(disagrees[name]
      ? ['Activity history could not be reconciled with this computer']
      : []);
    await watch(30_000);
    expect(backend.pages.mock.calls.length).toBe(pages);
    expect(backend.members.mock.calls.length).toBe(members);
    expect(shown.size).toBe(disagrees[name] ? 1 : 0);
    expect(projection.shape).toEqual(HISTORIES[name].members);
    expect(pane.threadId).toBe('t');
  });
});
