import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import {
  threadMachine, threadMachineUnreachable, threadMachineUnreachableSettled, verifiedThreadMachine,
} from './attachedBackends.svelte';
import {
  __setTransportStatusForTest, backendUnreachableSettled, UNREACHABLE_SETTLE_MS,
} from './transportStatus.svelte';
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

// Thread rows dim and relabel a computer's threads only once it has been
// unreachable for UNREACHABLE_SETTLE_MS without a break, so a spotty
// connection never flips them. The composer's answer does not wait.
describe('threadMachineUnreachableSettled', () => {
  let laptop: ReturnType<typeof stageBackend>;
  let probe: ReturnType<typeof probeReactivity<boolean>> | null = null;

  function stage(status: 'connected' | 'disconnected' = 'connected'): void {
    vi.useFakeTimers();
    resetStagedBackends();
    laptop = stageBackend({ id: 'laptop', backendId: REMOTE_BACKEND_UUID, status });
    noteThread('remote-thread', 'laptop', 0);
    probe = probeReactivity(() => threadMachineUnreachableSettled('remote-thread', null));
  }
  function settled(): boolean {
    flushSync();
    return probe!.latest;
  }

  afterEach(() => {
    probe?.dispose();
    probe = null;
    resetStagedBackends();
    vi.useRealTimers();
  });

  it('never dims for a drop shorter than the settle time', () => {
    stage();
    laptop.setStatus('reconnecting');
    expect(threadMachineUnreachable('remote-thread', null)).toBe(true);
    vi.advanceTimersByTime(UNREACHABLE_SETTLE_MS - 1);
    expect(settled()).toBe(false);
    laptop.setStatus('connected');
    vi.advanceTimersByTime(UNREACHABLE_SETTLE_MS * 4);
    expect(settled()).toBe(false);
  });

  it('dims once a drop lasts the settle time, and clears the moment it reconnects', () => {
    stage();
    laptop.setStatus('reconnecting');
    vi.advanceTimersByTime(UNREACHABLE_SETTLE_MS);
    expect(settled()).toBe(true);
    laptop.setStatus('connected');
    expect(settled()).toBe(false);
    expect(threadMachineUnreachable('remote-thread', null)).toBe(false);
    // The next drop counts from its own start.
    laptop.setStatus('reconnecting');
    vi.advanceTimersByTime(UNREACHABLE_SETTLE_MS - 1);
    expect(settled()).toBe(false);
    vi.advanceTimersByTime(1);
    expect(settled()).toBe(true);
  });

  it('never dims through repeated short drops', () => {
    stage();
    for (let i = 0; i < 6; i++) {
      laptop.setStatus('reconnecting');
      vi.advanceTimersByTime(UNREACHABLE_SETTLE_MS - 5_000);
      expect(settled()).toBe(false);
      laptop.setStatus('connected');
      vi.advanceTimersByTime(1_000);
      expect(settled()).toBe(false);
    }
  });

  it('counts one drop from its start through each attempt it reports, and leaves nothing armed past a reconnect', () => {
    stage();
    laptop.setStatus('reconnecting');
    vi.advanceTimersByTime(10_000);
    // The ladder reports each attempt while the computer stays down.
    laptop.setStatus('disconnected');
    laptop.setStatus('reconnecting');
    vi.advanceTimersByTime(UNREACHABLE_SETTLE_MS - 10_000);
    expect(settled()).toBe(true);
    laptop.setStatus('connected');
    laptop.setStatus('reconnecting');
    vi.advanceTimersByTime(10_000);
    laptop.setStatus('reconnecting');
    laptop.setStatus('connected');
    vi.advanceTimersByTime(UNREACHABLE_SETTLE_MS * 2);
    expect(settled()).toBe(false);
  });

  it('dims a computer that never connects the settle time after this window starts watching it', () => {
    stage('disconnected');
    vi.advanceTimersByTime(UNREACHABLE_SETTLE_MS - 1);
    expect(settled()).toBe(false);
    vi.advanceTimersByTime(1);
    expect(settled()).toBe(true);
  });

  it('forgets a detached computer, with nothing left to fire', () => {
    stage();
    laptop.setStatus('reconnecting');
    vi.advanceTimersByTime(UNREACHABLE_SETTLE_MS - 1);
    resetStagedBackends();
    vi.advanceTimersByTime(UNREACHABLE_SETTLE_MS * 2);
    expect(backendUnreachableSettled('laptop')).toBe(false);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('never dims on a page with one computer: the banner explains that outage', () => {
    vi.useFakeTimers();
    resetStagedBackends();
    __setTransportStatusForTest({ status: 'reconnecting', nextAttemptAt: null });
    vi.advanceTimersByTime(UNREACHABLE_SETTLE_MS);
    expect(backendUnreachableSettled(HOME_BACKEND)).toBe(true);
    expect(threadMachineUnreachableSettled('home-thread', null)).toBe(false);
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    expect(backendUnreachableSettled(HOME_BACKEND)).toBe(false);
  });
});
