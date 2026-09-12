// Exercise the actual model controls and transport router together. A mocked
// binding alone cannot reveal a remote project being sent to the home database.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Call } from '../transport/runtime';
import { wsClient, type WSClient } from '../transport/wsClient';
import { __attachBackendForTest, __resetBackendsForTest } from '../transport/backends';
import { HOME_BACKEND } from '../transport/backendKey';
import { HOME_DESCRIPTOR } from '../transport/manifestBackends';
import { __resetEntityIndexForTest, forgetProject, noteProject, threadBackend } from '../transport/entityIndex';
import { createThreadPane } from './thread.svelte';
import { focusPane, registerPaneForTest, resetPanesForTest } from './panes.svelte';
import { applyThreadModelSelection, applyThreadReasoningEffort } from './threadModelControls';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { makeThread } from '../../test/helpers/chat';
import type { Project, Thread } from '../types/models';
import type { ProviderID } from '../types/providers';

const REMOTE = 'remote-mac';
const UPDATE_DEFAULTS = 595194384;
const CREATE_THREAD = 2579322833;
const UPDATE_MODEL = 3140398729;

function client(callByID: WSClient['callByID']): WSClient {
  return {
    callByID,
    callByName: vi.fn(),
    subscribe: vi.fn(() => () => undefined),
    installStepUpProver: vi.fn(), setWatchedThreads: vi.fn(), setPresence: vi.fn(), setLease: vi.fn(),
    getStatus: vi.fn(() => ({ status: 'connected', nextAttemptAt: null })),
    onReplay: vi.fn(() => () => undefined),
    onStatusChange: vi.fn(() => () => undefined), getHello: vi.fn(() => null),
    onHelloChange: vi.fn(() => () => undefined), close: vi.fn(),
  } as unknown as WSClient;
}

beforeEach(() => {
  installThreadPaneTestEnv();
  resetPanesForTest();
  __resetBackendsForTest();
  __resetEntityIndexForTest();
  setBindingMock('UpdateNewThreadDefaults', (input: unknown) => Call.ByID(UPDATE_DEFAULTS, input));
  setBindingMock('CreateThread', (input: unknown) => Call.ByID(CREATE_THREAD, input));
  setBindingMock('UpdateThreadModelSelection', (...args: unknown[]) => Call.ByID(UPDATE_MODEL, ...args));
});

afterEach(() => {
  vi.restoreAllMocks();
  resetPanesForTest();
  __resetBackendsForTest();
  __resetEntityIndexForTest();
  __attachBackendForTest(HOME_DESCRIPTOR, wsClient);
});

function fixture(owner: string) {
  const project: Project = { id: `project-${owner || 'home'}`, name: 'Repo', path: '/repo', archived: false, sortPosition: 0, createdAt: 0, updatedAt: 0 };
  noteProject(project.id, owner);
  let materialized: Thread | undefined;
  const dispatch = (backend: string) => vi.fn(async (method: number, args: unknown[]) => {
    if (backend !== owner) throw new Error('The requested item no longer exists.');
    const input = args[0] as Record<string, unknown>;
    if (method === UPDATE_DEFAULTS) {
      expect(input.projectId).toBe(project.id);
      return { provider: input.provider, model: input.model, reasoningEffort: input.reasoningEffort ?? '', fastMode: false, contextWindow: 0, runtimeMode: 'full-access', branch: 'main', workspacePath: project.path };
    }
    if (method === CREATE_THREAD) {
      expect(input.projectId).toBe(project.id);
      materialized = makeThread({ ...input, id: `thread-${owner || 'home'}`, projectId: project.id, projectPath: project.path, workspacePath: project.path, isDraft: true } as Partial<Thread>);
      return materialized;
    }
    if (method === UPDATE_MODEL && materialized && args[0] === materialized.id) {
      materialized = { ...materialized, provider: args[1] as ProviderID, model: args[2] as string };
      return materialized;
    }
    throw new Error(`Unexpected method ${method}`);
  });
  const home = dispatch(HOME_BACKEND), remote = dispatch(REMOTE);
  __attachBackendForTest(HOME_DESCRIPTOR, client(home));
  __attachBackendForTest({ id: REMOTE, backendId: '99887766-5544-4333-8222-111111111111', name: 'Mac', wsUrl: '/ws/mac', bootstrapUrl: '/bootstrap/mac' }, client(remote));
  // Focus another computer: ownership must come from the control's project,
  // not the foreground pane or a remembered new-thread destination.
  const elsewhere = createThreadPane({ paneId: 'main' });
  const other: Project = { ...project, id: 'other-project' };
  noteProject(other.id, owner === REMOTE ? HOME_BACKEND : REMOTE);
  elsewhere.startDraftPlaceholder(other);
  registerPaneForTest('main', elsewhere);
  const pane = createThreadPane({ paneId: 'acting' });
  registerPaneForTest('acting', pane);
  focusPane('main');
  return { pane, project, home, remote };
}

describe('model changes follow the draft’s project computer', () => {
  for (const owner of [HOME_BACKEND, REMOTE]) {
    it.each(['claude', 'codex'] as const)(`switches %s on ${owner || 'home'} before and immediately after materialization`, async (initial) => {
      const { pane, project, home, remote } = fixture(owner);
      const next = initial === 'claude' ? 'codex' : 'claude';
      pane.startDraftPlaceholder(project, 'chat', { provider: initial, model: 'initial' });
      expect(await applyThreadModelSelection(pane, next, 'selected')).toEqual({ ok: true });
      expect(pane.hasDraftPlaceholder).toBe(true);
      expect(pane.thread?.provider).toBe(next);
      expect(pane.thread?.model).toBe('selected');
      const id = await pane.ensureMaterializedThread();
      expect(id).toBeTruthy();
      // No list refresh or thread event supplied this owner. Creation itself
      // must establish it before an immediate toolbar write can route.
      expect(threadBackend(id!)).toBe(owner);
      expect(await applyThreadModelSelection(pane, initial, 'materialized-selection')).toEqual({ ok: true });
      expect(pane.thread?.provider).toBe(initial);
      expect(pane.thread?.model).toBe('materialized-selection');
      expect(pane.thread?.isDraft).toBe(true);
      expect((owner === REMOTE ? home : remote).mock.calls).toEqual([]);
      expect((owner === REMOTE ? remote : home).mock.calls.map(([method]) => method)).toEqual([UPDATE_DEFAULTS, CREATE_THREAD, UPDATE_MODEL]);
    });
  }

  it('routes sibling defaults controls identically and refuses a removed project without touching home', async () => {
    const { pane, project, home, remote } = fixture(REMOTE);
    pane.startDraftPlaceholder(project, 'chat', { provider: 'codex', model: 'initial' });
    expect(await applyThreadReasoningEffort(pane, 'high')).toEqual({ ok: true });
    expect(pane.thread?.reasoningEffort).toBe('high');
    forgetProject(project.id);
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const result = await applyThreadModelSelection(pane, 'claude', 'new');
    expect(result.ok).toBe(false);
    expect(result.error).toContain('computer that owns');
    expect(home).not.toHaveBeenCalled();
    expect(remote).toHaveBeenCalledTimes(1);
  });
});
