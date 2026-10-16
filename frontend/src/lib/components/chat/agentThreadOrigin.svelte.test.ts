import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { agentThreadOriginChip, openAgentThreadOrigin } from './agentThreadOrigin';
import { REVEAL_PANE_EVENT } from '../../stores/eventNames';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import {
  createPane,
  getFocusedPaneId,
  getPane,
  openThreadInNewPane,
  openThreadInPane,
  resetPanesForTest,
} from '../../stores/panes.svelte';
import { replaceAllThreads } from '../../stores/threads.svelte';
import { applyThreadUpdated } from '../../stores/eventsThreadRows';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { installPaneMocks, makeThread } from '../../../test/helpers/chat';
import { stageBackend, resetStagedBackends, REMOTE_BACKEND_UUID } from '../../../test/helpers/backends';
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
    resetStagedBackends();
    stageBackend({ id: HOME_BACKEND, name: 'This computer', backendId: 'home-uuid' });
  });
  afterEach(() => {
    resetStagedBackends();
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
    resetPanesForTest();
    resetPaneLayoutForTest();
    installPaneMocks();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 600 }]);
    createPane('main');
  });
  afterEach(() => {
    resetPanesForTest();
    resetPaneLayoutForTest();
    replaceAllThreads([]);
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
