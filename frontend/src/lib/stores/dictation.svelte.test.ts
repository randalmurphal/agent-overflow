import { afterEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { hasScope } from '../transport/scopes';
import { __attachBackendForTest, __resetBackendsForTest, detachBackend } from '../transport/backends';
import {
  applyDictationSourcesChanged,
  dictationLiveReady,
  dictationSubscriptionReady,
  dictationSource,
  installDictationSourcesRefresh,
  refreshDictationSources,
  registerComposerDictation,
  toggleComposerDictation,
} from './dictation.svelte';
import type { DictationSession } from '../dictation/dictationSession.svelte';

vi.mock('../transport/scopes', async (original) => ({ ...await original<object>(), hasScope: vi.fn(() => true) }));

afterEach(() => {
  vi.mocked(hasScope).mockReset().mockReturnValue(true);
  __resetBackendsForTest();
});

const fakeClient = {
  callByID: async () => null, callByName: async () => null, subscribe: () => () => undefined,
  installStepUpProver: () => undefined, setLease: () => undefined, setWatchedThreads: () => undefined,
  getStatus: () => ({ status: 'connected', nextAttemptAt: null }), onStatusChange: () => () => undefined,
  getHello: () => null, onHelloChange: () => () => undefined, onReplay: () => () => undefined, close: () => undefined,
};

function attachLaptop(): void {
  __attachBackendForTest({
    id: 'laptop', backendId: '99999999-8888-4777-8666-555555555555', name: 'Laptop',
    wsUrl: 'ws://localhost:3000/ws/backend/laptop', bootstrapUrl: '/bootstrap/laptop.json',
  }, fakeClient as never);
}

/** DictationSources answers in the order the test releases them. */
function heldAnswers() {
  const answers: Array<(subscription: boolean) => void> = [];
  setBindingMock('DictationSources', () => new Promise((resolve) => {
    answers.push((subscription) => resolve({ subscription }));
  }));
  return answers;
}

describe('dictation availability', () => {
  it('lets the newest read win over an older one that answers later', async () => {
    attachLaptop();
    const answers = heldAnswers();
    const first = refreshDictationSources('laptop');
    const second = refreshDictationSources('laptop');
    await vi.waitFor(() => expect(answers).toHaveLength(2));
    answers[1](true);
    await second;
    answers[0](false);
    await first;
    expect(dictationSubscriptionReady('laptop')).toBe(true);
  });

  it('forgets a detached computer and drops its read in flight', async () => {
    attachLaptop();
    setBindingMock('DictationSources', async () => ({ subscription: true }));
    await refreshDictationSources('laptop');
    expect(dictationSubscriptionReady('laptop')).toBe(true);

    const answers = heldAnswers();
    const read = refreshDictationSources('laptop');
    await vi.waitFor(() => expect(answers).toHaveLength(1));
    detachBackend('laptop');
    answers[0](true);
    await read;
    expect(dictationSubscriptionReady('laptop')).toBe(false);
  });

  it('asks nothing without permission to dictate there, and forgets the old answer', async () => {
    attachLaptop();
    setBindingMock('DictationSources', async () => ({ subscription: true }));
    await refreshDictationSources('laptop');
    const read = setBindingMock('DictationSources', async () => ({ subscription: true }));
    vi.mocked(hasScope).mockReturnValue(false);
    await refreshDictationSources('laptop');
    expect(read).not.toHaveBeenCalled();
    expect(dictationSubscriptionReady('laptop')).toBe(false);
  });

  it('re-reads after an account change only a computer someone asked about', async () => {
    attachLaptop();
    const read = setBindingMock('DictationSources', async () => ({ subscription: false }));
    applyDictationSourcesChanged('laptop');
    expect(read).not.toHaveBeenCalled();

    await refreshDictationSources('laptop');
    expect(dictationSubscriptionReady('laptop')).toBe(false);
    read.mockImplementation(async () => ({ subscription: true }));
    applyDictationSourcesChanged('laptop');
    await vi.waitFor(() => expect(dictationSubscriptionReady('laptop')).toBe(true));
  });

  it('re-reads after a change that lands before the first answer', async () => {
    attachLaptop();
    const answers = heldAnswers();
    const first = refreshDictationSources('laptop');
    await vi.waitFor(() => expect(answers).toHaveLength(1));
    applyDictationSourcesChanged('laptop');
    await vi.waitFor(() => expect(answers).toHaveLength(2));
    answers[1](false);
    answers[0](true);
    await first;
    expect(dictationSubscriptionReady('laptop')).toBe(false);
  });

  it('chooses live where it is on and the engine streams, else the recording', async () => {
    attachLaptop();
    const read = setBindingMock('DictationSources', async () => ({ subscription: true, live: true }));
    const both = { records: true, streamsPcm: true };
    expect(dictationSource('laptop', both)).toBeNull();
    await refreshDictationSources('laptop');
    expect(dictationSubscriptionReady('laptop')).toBe(true);
    expect(dictationLiveReady('laptop')).toBe(true);
    expect(dictationSource('laptop', both)).toBe('live');
    expect(dictationSource('laptop', { records: true, streamsPcm: false })).toBe('recorded');
    expect(dictationSource('laptop', { records: false, streamsPcm: false })).toBeNull();

    read.mockImplementation(async () => ({ subscription: false, live: true }));
    await refreshDictationSources('laptop');
    expect(dictationSubscriptionReady('laptop')).toBe(false);
    expect(dictationLiveReady('laptop')).toBe(true);
    expect(dictationSource('laptop', { records: true, streamsPcm: false })).toBeNull();
    expect(dictationSource('laptop', { records: false, streamsPcm: true })).toBe('live');

    read.mockImplementation(async () => ({ subscription: false, live: false }));
    await refreshDictationSources('laptop');
    expect(dictationLiveReady('laptop')).toBe(false);
    expect(dictationSource('laptop', both)).toBeNull();
  });

  it('re-reads every computer asked about when the window comes back, and stops when removed', async () => {
    attachLaptop();
    const read = setBindingMock('DictationSources', async () => ({ subscription: false, live: false }));
    const remove = installDictationSourcesRefresh();
    window.dispatchEvent(new Event('focus'));
    expect(read).not.toHaveBeenCalled();

    await refreshDictationSources('laptop');
    read.mockImplementation(async () => ({ subscription: false, live: true }));
    window.dispatchEvent(new Event('focus'));
    await vi.waitFor(() => expect(dictationLiveReady('laptop')).toBe(true));
    expect(read).toHaveBeenCalledTimes(2);

    remove();
    window.dispatchEvent(new Event('focus'));
    document.dispatchEvent(new Event('visibilitychange'));
    expect(read).toHaveBeenCalledTimes(2);
  });
});

describe('composer dictation registry', () => {
  it("routes the chord to that pane's session and forgets it on release", () => {
    const toggle = vi.fn();
    const session = { toggle } as unknown as DictationSession;
    const release = registerComposerDictation('pane-1', 'laptop', session, () => true);
    expect(toggleComposerDictation('pane-1')).toBe(true);
    expect(toggleComposerDictation('pane-2')).toBe(false);
    expect(toggleComposerDictation(null)).toBe(false);
    expect(toggle).toHaveBeenCalledTimes(1);

    // A later session on the same pane is not released by the earlier one.
    const next = { toggle: vi.fn() } as unknown as DictationSession;
    const releaseNext = registerComposerDictation('pane-1', 'laptop', next, () => true);
    release();
    expect(toggleComposerDictation('pane-1')).toBe(true);
    releaseNext();
    expect(toggleComposerDictation('pane-1')).toBe(false);
  });

  it('never starts a session whose mic is not shown', () => {
    const toggle = vi.fn();
    let shown = false;
    const release = registerComposerDictation('pane-1', 'laptop', { toggle } as unknown as DictationSession, () => shown);
    expect(toggleComposerDictation('pane-1')).toBe(false);
    expect(toggle).not.toHaveBeenCalled();
    shown = true;
    expect(toggleComposerDictation('pane-1')).toBe(true);
    expect(toggle).toHaveBeenCalledTimes(1);
    release();
  });
});
