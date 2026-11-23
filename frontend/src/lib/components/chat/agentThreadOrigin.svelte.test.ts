import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { agentThreadOpener, agentThreadOriginChip, openAgentThreadOrigin } from './agentThreadOrigin';
import { REVEAL_PANE_EVENT } from '../../stores/eventNames';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import {
  createPane,
  findPaneShowingThread,
  focusPane,
  getFocusedPaneId,
  getPane,
  openThreadInNewPane,
  openThreadInPane,
} from '../../stores/panes.svelte';
import { AGENT_THREAD_COMPANION_KIND, agentThreadCompanionPlacer } from '../../stores/agentThreadCompanion';
import { companionForSource } from '../../stores/companionPanes.svelte';
import { replaceAllThreads } from '../../stores/threads.svelte';
import { applyThreadUpdated } from '../../stores/eventsThreadRows';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import { installPaneMocks, makeThread } from '../../../test/helpers/chat';
import { stageBackend, REMOTE_BACKEND_UUID } from '../../../test/helpers/backends';
import { HOME_BACKEND } from '../../transport/backendKey';
import type { UserMessageOriginThread } from '../../utils/userMessageMeta';

function origin(overrides: Partial<UserMessageOriginThread> = {}): UserMessageOriginThread {
  return {
    computerId: '',
    computerName: '',
    threadId: 'thread-9',
    title: 'Auth rewrite',
    token: 'tok-1',
    kind: '',
    ...overrides,
  };
}

// The chip is the only place a reader learns that an agent in another
// thread wrote a message they are about to read an answer to, so what it
// says — and whether it goes anywhere — is the contract here.
describe('agentThreadOriginChip', () => {
  beforeEach(() => {
    stageBackend({ id: HOME_BACKEND, name: 'This computer', backendId: 'home-uuid' });
  });

  it('names the thread alone when the row came from this computer', () => {
    const chip = agentThreadOriginChip(origin(), HOME_BACKEND);
    expect(chip.label).toBe('from Auth rewrite');
    expect(chip.open).not.toBeNull();
  });

  it('names the computer when the source is a different one, and opens it while it is reachable', () => {
    stageBackend({ id: 'studio', backendId: REMOTE_BACKEND_UUID, name: 'Studio' });
    const chip = agentThreadOriginChip(origin({ computerId: 'studio', computerName: 'Studio' }), HOME_BACKEND);
    expect(chip.label).toBe('from Auth rewrite on Studio');
    expect(chip.open).not.toBeNull();
  });

  it('matches the computer by its stable UUID as well as by its registry id', () => {
    stageBackend({ id: 'studio', backendId: REMOTE_BACKEND_UUID, name: 'Studio' });
    const chip = agentThreadOriginChip(
      origin({ computerId: REMOTE_BACKEND_UUID, computerName: 'Studio' }),
      HOME_BACKEND,
    );
    expect(chip.label).toBe('from Auth rewrite on Studio');
    expect(chip.open).not.toBeNull();
  });

  it('is inert with the same label when that computer is attached but its socket is down', () => {
    stageBackend({ id: 'studio', backendId: REMOTE_BACKEND_UUID, name: 'Studio', status: 'reconnecting' });
    const chip = agentThreadOriginChip(origin({ computerId: 'studio', computerName: 'Studio' }), HOME_BACKEND);
    expect(chip.label).toBe('from Auth rewrite on Studio');
    expect(chip.open).toBeNull();
  });

  it('is inert, and falls back to the name the wire carried, when the computer is not attached at all', () => {
    const chip = agentThreadOriginChip(
      origin({ computerId: 'unpaired-computer', computerName: 'Old laptop' }),
      HOME_BACKEND,
    );
    expect(chip.label).toBe('from Auth rewrite on Old laptop');
    expect(chip.open).toBeNull();
  });

  it('renders a thread with no title rather than an empty label', () => {
    const chip = agentThreadOriginChip(origin({ title: '' }), HOME_BACKEND);
    expect(chip.label).toBe('from another thread');
  });

  it('drops the suffix when the source computer is the one showing the row', () => {
    stageBackend({ id: 'studio', backendId: REMOTE_BACKEND_UUID, name: 'Studio' });
    // The pane is attached to Studio, and the row came from a Studio thread.
    const chip = agentThreadOriginChip(origin({ computerId: 'studio', computerName: 'Studio' }), 'studio');
    expect(chip.label).toBe('from Auth rewrite');
    expect(chip.open).not.toBeNull();
  });
});

describe('openAgentThreadOrigin', () => {
  beforeEach(() => {
    resetPaneLayoutForTest();
    installPaneMocks();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 600 }]);
    createPane('main');
  });
  afterEach(() => {
    resetPaneLayoutForTest();
    replaceAllThreads([]);
    for (const toast of getToasts()) removeToast(toast.id);
  });

  it('reveals the pane already showing the thread, so a compact strip moves to it', async () => {
    const shown = makeThread({ id: 'thread-9', title: 'Auth rewrite' });
    const here = makeThread({ id: 'thread-1', title: 'Here' });
    replaceAllThreads([shown, here]);
    const pane = await openThreadInNewPane(shown);
    await openThreadInPane(here, 'main');
    const revealed: string[] = [];
    const onReveal = ((event: CustomEvent<{ paneId: string }>) => {
      revealed.push(event.detail.paneId);
    }) as EventListener;
    window.addEventListener(REVEAL_PANE_EVENT, onReveal);
    try {
      await openAgentThreadOrigin(origin(), HOME_BACKEND);
    } finally {
      window.removeEventListener(REVEAL_PANE_EVENT, onReveal);
    }
    expect(getFocusedPaneId()).toBe(pane.paneId);
    expect(revealed).toEqual([pane.paneId]);
  });

  // The row this client does not hold is read, and a frame for it that
  // lands during the read reaches no list or pane: the pane ends on the
  // newer row, not the read's.
  it('keeps a row a frame brought during the read over the older read', async () => {
    const read = makeThread({ id: 'thread-9', title: 'Auth rewrite' });
    const renamed = { ...read, title: 'Renamed meanwhile' };
    setBindingMock('SwitchThread', async () => read);
    let reads = 0;
    setBindingMock('GetThread', async () => {
      reads += 1;
      if (reads > 1) return renamed;
      applyThreadUpdated({ action: 'full', thread: renamed });
      return read;
    });
    await openAgentThreadOrigin(origin(), HOME_BACKEND, (thread) => openThreadInPane(thread, 'main'));
    expect(getPane('main')?.thread?.title).toBe('Renamed meanwhile');
    expect(reads).toBe(2);
  });

  it('keeps a renamed title a patch brought during the read over the older read', async () => {
    const read = makeThread({ id: 'thread-9', title: 'Auth rewrite' });
    setBindingMock('SwitchThread', async () => read);
    let reads = 0;
    setBindingMock('GetThread', async () => {
      reads += 1;
      if (reads > 1) return { ...read, title: 'Renamed meanwhile' };
      applyThreadUpdated({ action: 'patch', id: 'thread-9', title: 'Renamed meanwhile' });
      return read;
    });
    await openAgentThreadOrigin(origin(), HOME_BACKEND, (thread) => openThreadInPane(thread, 'main'));
    expect(getPane('main')?.thread?.title).toBe('Renamed meanwhile');
    expect(reads).toBe(2);
  });

  it('reports a failed second read as a failed refresh of the thread it opened', async () => {
    const read = makeThread({ id: 'thread-9', title: 'Auth rewrite' });
    setBindingMock('SwitchThread', async () => read);
    let reads = 0;
    setBindingMock('GetThread', async () => {
      reads += 1;
      if (reads > 1) throw new Error('connection lost');
      applyThreadUpdated({ action: 'full', thread: { ...read, title: 'Renamed meanwhile' } });
      return read;
    });
    await openAgentThreadOrigin(origin(), HOME_BACKEND, (thread) => openThreadInPane(thread, 'main'));
    expect(getPane('main')?.threadId).toBe('thread-9');
    const messages = getToasts().map((toast) => toast.message);
    expect(messages.some((message) => message.startsWith('Opened Auth rewrite, but could not refresh it'))).toBe(true);
    expect(messages.some((message) => message.startsWith('Could not open'))).toBe(false);
  });

  // The read can outlast what the click was made from: the pane moves to
  // another thread, or focus to another pane. The thread then opens nowhere
  // rather than beside, or in place of, a thread the click never saw.
  function deferredRead(thread: ReturnType<typeof makeThread>) {
    let resolve!: () => void;
    setBindingMock('GetThread', () => new Promise((done) => { resolve = () => done(thread); }));
    return () => resolve();
  }

  it('opens a worker beside nothing when its source pane moved to another thread during the read', async () => {
    const a = makeThread({ id: 'thread-a', title: 'A' });
    const b = makeThread({ id: 'thread-b', title: 'B' });
    const worker = makeThread({ id: 'thread-9', title: 'Auth rewrite' });
    replaceAllThreads([a, b]);
    setBindingMock('SwitchThread', async (id: string) => [a, b, worker].find((t) => t.id === id));
    await openThreadInPane(a, 'main');
    const release = deferredRead(worker);
    const source = getPane('main')!;
    agentThreadOpener(origin(), HOME_BACKEND, () => agentThreadCompanionPlacer(source)).open!();
    await openThreadInPane(b, 'main');
    release();
    await vi.waitFor(() => expect(getToasts().map((toast) => toast.message)).toContain(
      'Could not open Auth rewrite: The pane moved on before the thread opened.',
    ));
    expect(companionForSource('main', AGENT_THREAD_COMPANION_KIND)).toBeNull();
    expect(findPaneShowingThread('thread-9')).toBeNull();
    expect(getPane('main')?.threadId).toBe('thread-b');
  });

  it('opens nothing in the focused pane when focus moved to another pane during the read', async () => {
    const a = makeThread({ id: 'thread-a', title: 'A' });
    const c = makeThread({ id: 'thread-c', title: 'C' });
    const worker = makeThread({ id: 'thread-9', title: 'Auth rewrite' });
    replaceAllThreads([a, c]);
    setBindingMock('SwitchThread', async (id: string) => [a, c, worker].find((t) => t.id === id));
    setPaneLayoutItemsForTest([
      { id: 'main', paneId: 'main', kind: 'thread', widthPx: 600 },
      { id: 'side', paneId: 'side', kind: 'thread', widthPx: 600 },
    ]);
    createPane('side');
    await openThreadInPane(c, 'side');
    await openThreadInPane(a, 'main');
    focusPane('main');
    const release = deferredRead(worker);
    agentThreadOriginChip(origin(), HOME_BACKEND).open!();
    focusPane('side');
    release();
    await vi.waitFor(() => expect(getToasts().map((toast) => toast.message)).toContain(
      'Could not open Auth rewrite: The pane moved on before the thread opened.',
    ));
    expect(getPane('side')?.threadId).toBe('thread-c');
    expect(getPane('main')?.threadId).toBe('thread-a');
  });

  // An ask's copy is deleted once it answers; a click that raced the
  // delete reads nothing, opens nothing beside the source and says so by
  // the name the row gave it.
  it('opens no companion for a thread deleted before its read, and names it in the toast', async () => {
    const a = makeThread({ id: 'thread-a', title: 'A' });
    replaceAllThreads([a]);
    setBindingMock('SwitchThread', async () => a);
    await openThreadInPane(a, 'main');
    setBindingMock('GetThread', async () => { throw new Error('The requested item no longer exists.'); });
    const source = getPane('main')!;
    agentThreadOpener({ ...origin(), title: 'the copy asking Release notes' }, HOME_BACKEND, () => agentThreadCompanionPlacer(source)).open!();
    await vi.waitFor(() => expect(getToasts().map((toast) => toast.message)).toContain(
      'Could not open the copy asking Release notes: The requested item no longer exists.',
    ));
    expect(companionForSource('main', AGENT_THREAD_COMPANION_KIND)).toBeNull();
    expect(getPane('main')?.threadId).toBe('thread-a');
  });

  it('opens in the pane it was clicked from when nothing moved during the read', async () => {
    const a = makeThread({ id: 'thread-a', title: 'A' });
    const worker = makeThread({ id: 'thread-9', title: 'Auth rewrite' });
    replaceAllThreads([a]);
    setBindingMock('SwitchThread', async (id: string) => [a, worker].find((t) => t.id === id));
    await openThreadInPane(a, 'main');
    focusPane('main');
    const release = deferredRead(worker);
    agentThreadOriginChip(origin(), HOME_BACKEND).open!();
    release();
    await vi.waitFor(() => expect(getPane('main')?.threadId).toBe('thread-9'));
  });

  it('reads a row once when no frame lands during the read', async () => {
    const read = makeThread({ id: 'thread-9', title: 'Auth rewrite' });
    setBindingMock('SwitchThread', async () => read);
    let reads = 0;
    setBindingMock('GetThread', async () => {
      reads += 1;
      return read;
    });
    await openAgentThreadOrigin(origin(), HOME_BACKEND, (thread) => openThreadInPane(thread, 'main'));
    expect(getPane('main')?.thread?.title).toBe('Auth rewrite');
    expect(reads).toBe(1);
  });
});
