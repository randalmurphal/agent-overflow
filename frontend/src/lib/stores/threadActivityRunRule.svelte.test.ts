// The pane groups a thread's agent messages by the membership rule its
// owning backend advertises (`activity-runs.agent-message-members.v1`), and
// asks for that rule exactly when it is advertised. Against each kind of
// backend it settles on that backend's runs with a bounded number of pages
// and members calls and a held window it can describe; a hello that changes
// the rule regroups the pane, and every agent pane and agent card of that
// computer, through the reconnect refresh, partial windows included; a
// window read under the other rule is never offered as held; and a
// disagreement no reload can settle ends in one visible failure.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { activityRunProse, activityRunRow } from '../../test/helpers/activityRuns';
import { serveClassifiedHistory, serveWindowedHistory, type AgentMessageRuleOf } from '../../test/helpers/activityRunBackend';
import { stageBackend } from '../../test/helpers/backends';
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
import { registerPaneForTest } from './panes.svelte';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import { createAgentScopeView, type AgentScopeView } from './agentScopeView.svelte';
import { timelinePageShape } from './threadPaneShared';
import { AGENT_MESSAGE_MEMBERS_RULE, AGENT_MESSAGES_ABSORBED_RULE } from '../utils/activityRunSpans';
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
    capabilities: ['timeline.scopes.v1', 'timeline.digests.v1', ...(members ? [AGENT_MESSAGE_RUN_MEMBERS_CAPABILITY] : [])],
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
    // The refresh re-reads the sidebar, which does not list the open thread,
    // so it asks the thread's computer, which still holds it.
    setBindingMock('GetThread', async (id: string) => makeThread({ id }));
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

describe('a rule change over a partial window', () => {
  // A prose row, an agent message and a run of 100 tools longer than a
  // page ships of it, then an answer. Under the member rule the message
  // opens the run and is not shipped; under the absorbed rule it is a
  // shipped row and the run starts at the first tool.
  const TOOLS = 100;
  const tools = (base: number) => Array.from({ length: TOOLS }, (_, i) => activityRunRow(`t${i}`, base + i));
  const runShape = (count: number) => {
    const loaded = timelinePageShape().runWindowRows;
    return `run[t${TOOLS - loaded}..t${TOOLS - 1}/${count}]`;
  };
  const shapes = (lead: string[]) => ({
    members: () => [...lead, runShape(TOOLS + 1), 'a'],
    absorbed: () => [...lead, 'm', runShape(TOOLS), 'a'],
  });

  /** Each run as its loaded edges and the member count its stub supplies. */
  function windowShape(nodes: readonly TimelineNode[], pane: ThreadPane): string[] {
    return nodes.map(node => {
      if (node.kind === 'leaf') return node.item.id;
      if (node.kind !== 'activity_run') return node.kind;
      const ids = node.children.map(child => child.kind === 'leaf' ? child.item.id : child.kind);
      const count = pane.activityRuns.summaryFacts(node.runId)?.memberCount ?? ids.length;
      return `run[${ids[0]}..${ids.at(-1)}/${count}]`;
    });
  }
  function mountWindowShape(pane: ThreadPane): { readonly shape: string[]; readonly runIds: string[] } {
    let nodes: TimelineNode[] = [];
    const dispose = $effect.root(() => {
      const projection = createTimelineRowProjection({ getPane: () => pane });
      $effect(() => { nodes = projection.revealedNodes; });
    });
    cleanup.push(dispose);
    return {
      get shape() { return windowShape(nodes, pane); },
      get runIds() { return nodes.flatMap(node => node.kind === 'activity_run' ? [node.runId] : []); },
    };
  }

  function stageGpu(members: boolean) {
    const gpu = stageBackend({ id: 'gpu', hello: hello('gpu', members) });
    setCarriedSessionScopes('gpu', ['threads:read']);
    noteThread('t', 'gpu');
    setBindingMock('ListThreadLiveActivity', async () => []);
    setBindingMock('GetRateLimitsSnapshots', async () => []);
    setBindingMock('ListUnsettledFlushes', async () => []);
    setBindingMock('GetThread', async (id: string) => makeThread({ id }));
    return gpu;
  }

  async function openPane(): Promise<ThreadPane> {
    const pane = createThreadPane({ paneId: 'main' });
    registerPaneForTest('main', pane);
    await pane.switchThread(makeThread({ id: 't' }));
    cleanup.push(() => pane.clear());
    return pane;
  }

  it.each([true, false])('regroups a root pane both ways (member rule first: %s)', async (membersFirst) => {
    const gpu = stageGpu(membersFirst);
    const rows = [activityRunProse('p', 0), agentMessage('m', 1), ...tools(2), activityRunProse('a', TOOLS + 2)];
    serveWindowedHistory({ rows: root => root === '' ? rows : [] }, requestedRule);
    const projection = mountWindowShape(await openPane());
    cleanup.push(installComputerHydration());
    await vi.advanceTimersByTimeAsync(5000);
    const expected = shapes(['p']);
    expect(projection.shape).toEqual(membersFirst ? expected.members() : expected.absorbed());
    for (const members of [!membersFirst, membersFirst]) {
      gpu.setHello(hello('gpu', members));
      await vi.advanceTimersByTimeAsync(5000);
      expect(projection.shape).toEqual(members ? expected.members() : expected.absorbed());
    }
    expect(getToasts()).toEqual([]);
  });

  it.each([true, false])('does not cache a restored window across a rule change (member rule first: %s)', async (membersFirst) => {
    const gpu = stageGpu(membersFirst);
    noteThread('other', 'gpu');
    const rows = [activityRunProse('p', 0), agentMessage('m', 1), ...tools(2), activityRunProse('a', TOOLS + 2)];
    const backend = serveWindowedHistory({ rows: root => root === '' ? rows : [] }, requestedRule);
    const pane = await openPane();
    const projection = mountWindowShape(pane);
    await vi.advanceTimersByTimeAsync(5000);
    // Away and back: the cached window paints and its held window verifies.
    await pane.switchThread(makeThread({ id: 'other' }));
    await pane.switchThread(makeThread({ id: 't' }));
    await vi.advanceTimersByTimeAsync(5000);
    await expect(backend.syncs.mock.results.at(-1)?.value).resolves.toMatchObject({ status: 'fresh' });

    gpu.setHello(hello('gpu', !membersFirst));
    expect(pane.activityRuns.snapshotStubs()).toBeNull();
    expect(pane.activityRuns.heldRunFold()).toBeNull();
    // Away and back again: nothing of the other rule was cached to paint.
    await pane.switchThread(makeThread({ id: 'other' }));
    await pane.switchThread(makeThread({ id: 't' }));
    await vi.advanceTimersByTimeAsync(5000);
    const expected = shapes(['p']);
    expect(projection.shape).toEqual(membersFirst ? expected.absorbed() : expected.members());
  });

  // An agent pane reads the agent's whole transcript; a completed agent's
  // card reads its execution digest, whose prompt is the agent message.
  const launch = (id: string, index: number) => activityRunRow(id, index, { toolName: 'Agent', status: 'completed' });
  const rootRows = [launch('agent', 0), launch('card-agent', 1),
    activityRunRow('card-agent-done', 2, { kind: 'tool_completion', completionOf: 'card-agent', status: 'completed' })];
  const childOf = (parentId: string) => (row: Item): Item => ({ ...row, parentId });
  const agentRows = [activityRunProse('p', 10), agentMessage('m', 11), ...tools(12), activityRunProse('a', TOOLS + 12)]
    .map(childOf('agent'));
  const cardRows = [agentMessage('m', 300), ...tools(301), activityRunProse('a', TOOLS + 301)].map(childOf('card-agent'));
  const scopedHistory = {
    rows: (root: string) => root === '' ? rootRows : root === 'agent' ? agentRows : root === 'card-agent' ? cardRows : [],
    scope: (selection: { scopeRootId?: string; digestItemId?: string }) => {
      const root = rootRows.find(row => row.id === selection.scopeRootId);
      if (!root) return undefined;
      if (!selection.digestItemId) return { root, lifecycle: root };
      return { root, lifecycle: root, completion: rootRows[2], digest: { promptId: 'm', answerId: 'a' } };
    },
  };
  const SURFACES = {
    'agent pane': {
      open: (pane: ThreadPane) => createAgentScopeView(pane, 'agent', { viewKey: 'agent', openAgentPane: () => {} }),
      shapes: shapes(['p']),
    },
    'completed agent card': {
      open: (pane: ThreadPane) => createAgentScopeView(pane, 'card-agent',
        { viewKey: 'card:card-agent', digestItemId: 'card-agent-done', openAgentPane: () => {} }),
      shapes: shapes([]),
    },
  };
  type Surface = keyof typeof SURFACES;

  async function openSurface(name: Surface): Promise<{ view: AgentScopeView; projection: ReturnType<typeof mountWindowShape> }> {
    const view = SURFACES[name].open(await openPane());
    view.start();
    cleanup.push(() => view.dispose());
    return { view, projection: mountWindowShape(view.pane) };
  }

  const scopedSyncs = (backend: ReturnType<typeof serveWindowedHistory>) => backend.syncs.mock.calls.filter(call =>
    (call[1] as { selection?: { scopeRootId?: string } }).selection?.scopeRootId);

  describe.each(Object.keys(SURFACES) as Surface[])('an open %s', (name) => {
    it.each([true, false])('regroups both ways through the reconnect refresh (member rule first: %s)', async (membersFirst) => {
      const gpu = stageGpu(membersFirst);
      const backend = serveWindowedHistory(scopedHistory, requestedRule);
      const { projection } = await openSurface(name);
      cleanup.push(installComputerHydration());
      await vi.advanceTimersByTimeAsync(5000);
      const expected = SURFACES[name].shapes;
      expect(projection.shape).toEqual(membersFirst ? expected.members() : expected.absorbed());
      for (const members of [!membersFirst, membersFirst]) {
        const before = scopedSyncs(backend).length;
        gpu.setHello(hello('gpu', members));
        await vi.advanceTimersByTimeAsync(5000);
        expect(scopedSyncs(backend).length).toBeGreaterThan(before);
        expect(projection.shape).toEqual(members ? expected.members() : expected.absorbed());
      }
      expect(getToasts()).toEqual([]);
    });
  });

  it.each([true, false])('answers a refresh after a rule change with a page, not fresh (member rule first: %s)', async (membersFirst) => {
    const gpu = stageGpu(membersFirst);
    const backend = serveWindowedHistory(scopedHistory, requestedRule);
    const { view, projection } = await openSurface('agent pane');
    await vi.advanceTimersByTimeAsync(5000);
    // Under the rule it was read by, the held window verifies.
    await view.pane.refreshFromBackend(true);
    await vi.advanceTimersByTimeAsync(5000);
    await expect(backend.syncs.mock.results.at(-1)?.value).resolves.toMatchObject({ status: 'fresh' });

    // The rule changes under the open view, with no reconnect refresh.
    gpu.setHello(hello('gpu', !membersFirst));
    expect(view.pane.activityRuns.heldRunFold()).toBeNull();
    expect(view.pane.activityRuns.snapshotStubs()).toBeNull();
    const calls = scopedSyncs(backend).length;
    await view.pane.refreshFromBackend(true);
    await vi.advanceTimersByTimeAsync(5000);
    const sent = scopedSyncs(backend).slice(calls);
    expect(sent.length).toBeGreaterThan(0);
    for (const [, req] of sent) expect((req as { haveWindow?: unknown }).haveWindow).toBeUndefined();
    const expected = SURFACES['agent pane'].shapes;
    expect(projection.shape).toEqual(membersFirst ? expected.absorbed() : expected.members());
    // Read under the new rule, the window is offered again.
    expect(view.pane.activityRuns.heldRunFold()).not.toBeNull();
  });

  // A read sent under one rule whose answer lands after the hello moved to
  // the other: whatever it installs is labeled with the rule it asked for.
  describe('a read in flight across the change', () => {
    const ruleOf = (members: boolean) => members ? AGENT_MESSAGE_MEMBERS_RULE : AGENT_MESSAGES_ABSORBED_RULE;
    type Backend = ReturnType<typeof serveWindowedHistory>;
    type Mock = ReturnType<typeof vi.fn>;
    const scoped = (options: unknown) =>
      Boolean((options as { selection?: { scopeRootId?: string } } | undefined)?.selection?.scopeRootId);

    /**
     * Hold the answer of the next call `matches` accepts until released,
     * answering it as the backend would answer `ask(args)`.
     */
    function holdNext(mock: Mock, matches: (args: unknown[]) => boolean = () => true,
      ask: (args: unknown[]) => unknown[] = args => args) {
      const answer = mock.getMockImplementation() as (...args: unknown[]) => unknown;
      let made = false;
      let value: unknown;
      let release!: () => void;
      const gate = new Promise<void>(resolve => { release = resolve; });
      mock.mockImplementation(async (...args: unknown[]) => {
        if (made || !matches(args)) return answer(...args);
        made = true;
        value = await answer(...ask(args));
        await gate;
        return value;
      });
      return { get made() { return made; }, get value() { return value; }, release: () => release() };
    }

    function expectNotOfferedUnder(pane: ThreadPane, members: boolean) {
      expect(pane.activityRuns.windowReadRule()).not.toBe(ruleOf(members));
      expect(pane.activityRuns.heldRunFold()).toBeNull();
      expect(pane.activityRuns.snapshotStubs()).toBeNull();
    }

    function expectReadUnder(pane: ThreadPane, members: boolean) {
      expect(pane.activityRuns.windowReadRule()).toBe(ruleOf(members));
      expect(pane.activityRuns.heldRunFold()).not.toBeNull();
      expect(pane.activityRuns.snapshotStubs()).not.toBeNull();
    }

    const rootRows = () => [activityRunProse('p', 0), agentMessage('m', 1), ...tools(2), activityRunProse('a', TOOLS + 2)];
    // Each surface's whole-window read: a root pane refreshes with a
    // page around its anchor, a scoped timeline with a window sync, which
    // `changed` answers as if the history moved since the held window so
    // it carries a page.
    const SURFACES_IN_FLIGHT = {
      'root pane': {
        open: async () => {
          const pane = await openPane();
          return { pane, projection: mountWindowShape(pane) };
        },
        history: () => ({ rows: (root: string) => root === '' ? rootRows() : [] }),
        windowReads: (backend: Backend) => backend.pages as Mock,
        isWindowRead: (args: unknown[]) => !scoped(args[3]),
        changed: (args: unknown[]) => args,
        page: (answer: unknown) => answer,
        shapes: shapes(['p']),
      },
      'agent pane': {
        open: async () => {
          const { view, projection } = await openSurface('agent pane');
          return { pane: view.pane, projection };
        },
        history: () => scopedHistory,
        windowReads: (backend: Backend) => backend.syncs,
        isWindowRead: (args: unknown[]) => scoped(args[1]),
        changed: ([threadId, req]: unknown[]) => [threadId, { ...(req as object), haveWindow: undefined }],
        page: (answer: unknown) => (answer as { page?: unknown }).page,
        shapes: SURFACES['agent pane'].shapes,
      },
    };

    describe.each(Object.keys(SURFACES_IN_FLIGHT) as (keyof typeof SURFACES_IN_FLIGHT)[])('on a %s', (name) => {
      const surface = SURFACES_IN_FLIGHT[name];
      const sentReads = (backend: Backend) =>
        surface.windowReads(backend).mock.calls.filter(args => surface.isWindowRead(args)).length;

      // The change's refresh waits for the window read in flight, so that
      // read's answer always lands first; the other order is not reachable.
      it.each([true, false])('labels a window page with the rule it asked for (member rule first: %s)', async (membersFirst) => {
        const gpu = stageGpu(membersFirst);
        const backend = serveWindowedHistory(surface.history(), requestedRule);
        const { pane, projection } = await surface.open();
        cleanup.push(installComputerHydration());
        await vi.advanceTimersByTimeAsync(5000);
        const expected = surface.shapes;
        expect(projection.shape).toEqual(membersFirst ? expected.members() : expected.absorbed());

        const inFlight = holdNext(surface.windowReads(backend), surface.isWindowRead, surface.changed);
        void pane.refreshFromBackend(true).catch(() => {});
        await vi.advanceTimersByTimeAsync(100);
        expect(surface.page(inFlight.value)).toBeTruthy();
        const sent = sentReads(backend);
        gpu.setHello(hello('gpu', !membersFirst));
        await vi.advanceTimersByTimeAsync(1000);
        expect(sentReads(backend)).toBe(sent);

        const flipRefresh = holdNext(surface.windowReads(backend), surface.isWindowRead);
        inFlight.release();
        await vi.advanceTimersByTimeAsync(1000);
        expect(flipRefresh.made).toBe(true);
        // The old rule's rows are installed, labeled with the old rule, and
        // their stubs are not folded under the new one.
        expectNotOfferedUnder(pane, !membersFirst);
        expect(pane.activityRuns.windowReadRule()).toBe(ruleOf(membersFirst));

        flipRefresh.release();
        await vi.advanceTimersByTimeAsync(5000);
        expect(projection.shape).toEqual(membersFirst ? expected.absorbed() : expected.members());
        expectReadUnder(pane, !membersFirst);
        expect(getToasts()).toEqual([]);
      });

      // A members call is not serialized with window reads, so its answer
      // can land before the change's refresh or after it.
      describe.each([['the members answer', true], ["the change's refresh", false]] as const)('when %s lands first', (_label, membersAnswerFirst) => {
        it.each([true, false])('drops a members answer read under the old rule (member rule first: %s)', async (membersFirst) => {
          const gpu = stageGpu(membersFirst);
          const backend = serveWindowedHistory(surface.history(), requestedRule);
          const { pane, projection } = await surface.open();
          cleanup.push(installComputerHydration());
          await vi.advanceTimersByTimeAsync(5000);
          const expected = surface.shapes;
          expect(projection.runIds).toHaveLength(1);
          const ids = () => pane.items.map(item => item.id);

          const inFlight = holdNext(backend.members);
          const answered = pane.activityRuns.fetchMembers(projection.runIds[0], { direction: 'before', limit: 10 });
          await vi.advanceTimersByTimeAsync(100);
          expect(inFlight.made).toBe(true);
          const flipRefresh = holdNext(surface.windowReads(backend), surface.isWindowRead);
          gpu.setHello(hello('gpu', !membersFirst));
          await vi.advanceTimersByTimeAsync(1000);
          expect(flipRefresh.made).toBe(true);

          if (!membersAnswerFirst) {
            flipRefresh.release();
            await vi.advanceTimersByTimeAsync(5000);
            expect(projection.shape).toEqual(membersFirst ? expected.absorbed() : expected.members());
            expectReadUnder(pane, !membersFirst);
          }
          const before = ids();
          const reads = sentReads(backend);
          inFlight.release();
          // Dropped: it mounts no rows, folds no stub and asks for no reload.
          expect(await answered).toEqual([]);
          await vi.advanceTimersByTimeAsync(100);
          expect(ids()).toEqual(before);
          expect(sentReads(backend)).toBe(reads);
          if (membersAnswerFirst) flipRefresh.release();
          await vi.advanceTimersByTimeAsync(5000);
          expect(projection.shape).toEqual(membersFirst ? expected.absorbed() : expected.members());
          expectReadUnder(pane, !membersFirst);
          expect(getToasts()).toEqual([]);
        });
      });
    });
  });
});