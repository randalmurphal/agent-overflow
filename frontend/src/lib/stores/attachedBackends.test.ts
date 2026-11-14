import { afterEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import { threadMachine, verifiedThreadMachine } from './attachedBackends.svelte';
import { draftPlaceholderId } from './draftPlaceholderId';
import { __resetEntityIndexForTest, noteProject, noteThread } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
import { REMOTE_BACKEND_UUID, resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { probeReactivity } from '../../test/helpers/reactivity.svelte';

afterEach(__resetEntityIndexForTest);

it('places a draft placeholder on the computer of the project its id names', () => {
  noteProject('remote-project', 'laptop');
  const draft = draftPlaceholderId('main', 'remote-project', 'chat');
  expect(threadMachine(draft, null)).toBe('laptop');
  expect(threadMachine(draft, 'remote-project')).toBe('laptop');
});

it('places a thread by its own row before any project', () => {
  noteProject('remote-project', 'laptop');
  noteThread('thread-1', HOME_BACKEND, 0);
  expect(threadMachine('thread-1', 'remote-project')).toBe(HOME_BACKEND);
  expect(threadMachine('unknown-thread', 'remote-project')).toBe('laptop');
});

describe('verifiedThreadMachine', () => {
  const DESK_UUID = '11111111-2222-4333-8444-555555555555';

  function stageTwo(): void {
    resetStagedBackends();
    stageBackend({ id: 'laptop', backendId: REMOTE_BACKEND_UUID });
    stageBackend({ id: 'desk', backendId: DESK_UUID });
  }

  afterEach(resetStagedBackends);

  it('answers a known owner', () => {
    stageTwo();
    noteThread('owned', 'desk', 2);
    expect(verifiedThreadMachine('owned')).toBe('desk');
  });

  it('answers a draft placeholder through the project its id names', () => {
    stageTwo();
    noteProject('remote-project', 'laptop');
    expect(verifiedThreadMachine(draftPlaceholderId('main', 'remote-project', 'chat'))).toBe('laptop');
    expect(verifiedThreadMachine(draftPlaceholderId('main', 'unknown-project', 'chat'))).toBeNull();
  });

  it('answers the sole attached computer for an unknown owner, and no computer with several', () => {
    resetStagedBackends();
    // The page's own computer is the one attached.
    expect(verifiedThreadMachine('unknown')).toBe(HOME_BACKEND);
    stageBackend({ id: 'laptop', backendId: REMOTE_BACKEND_UUID });
    // Never HOME, which `threadMachine` answers for display.
    expect(threadMachine('unknown', null)).toBe(HOME_BACKEND);
    expect(verifiedThreadMachine('unknown')).toBeNull();
  });

  it('answers no computer for a thread two computers claim at one epoch', () => {
    stageTwo();
    noteThread('contested', 'laptop', 3);
    noteThread('contested', 'desk', 3);
    expect(threadMachine('contested', null)).toBe('laptop');
    expect(verifiedThreadMachine('contested')).toBeNull();
    // A higher epoch settles it.
    noteThread('contested', 'desk', 4);
    expect(verifiedThreadMachine('contested')).toBe('desk');
  });

  it('follows attach, detach and ownership reactively', () => {
    resetStagedBackends();
    const probe = probeReactivity(() => verifiedThreadMachine('thread'));
    try {
      expect(probe.latest).toBe(HOME_BACKEND);
      stageBackend({ id: 'laptop', backendId: REMOTE_BACKEND_UUID });
      flushSync();
      expect(probe.latest).toBeNull();
      noteThread('thread', 'laptop', 0);
      flushSync();
      expect(probe.latest).toBe('laptop');
      noteThread('thread', HOME_BACKEND, 0);
      flushSync();
      expect(probe.latest).toBeNull();
      resetStagedBackends();
      flushSync();
      expect(probe.latest).toBe(HOME_BACKEND);
    } finally {
      probe.dispose();
    }
  });
});
