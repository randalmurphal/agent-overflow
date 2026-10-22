import { afterEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { hasScope } from '../transport/scopes';
import { __attachBackendForTest, __resetBackendsForTest, detachBackend } from '../transport/backends';
import {
  applyDictationSourcesChanged,
  dictationAvailable,
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
    expect(dictationAvailable('laptop')).toBe(true);
  });

  it('forgets a detached computer and drops its read in flight', async () => {
    attachLaptop();
    setBindingMock('DictationSources', async () => ({ subscription: true }));
    await refreshDictationSources('laptop');
    expect(dictationAvailable('laptop')).toBe(true);

    const answers = heldAnswers();
    const read = refreshDictationSources('laptop');
    await vi.waitFor(() => expect(answers).toHaveLength(1));
    detachBackend('laptop');
    answers[0](true);
    await read;
    expect(dictationAvailable('laptop')).toBe(false);
  });

  it('asks nothing without permission to dictate there, and forgets the old answer', async () => {
    attachLaptop();
    setBindingMock('DictationSources', async () => ({ subscription: true }));
    await refreshDictationSources('laptop');
    const read = setBindingMock('DictationSources', async () => ({ subscription: true }));
    vi.mocked(hasScope).mockReturnValue(false);
    await refreshDictationSources('laptop');
    expect(read).not.toHaveBeenCalled();
    expect(dictationAvailable('laptop')).toBe(false);
  });

  it('re-reads after an account change only a computer someone asked about', async () => {
    attachLaptop();
    const read = setBindingMock('DictationSources', async () => ({ subscription: false }));
    applyDictationSourcesChanged('laptop');
    expect(read).not.toHaveBeenCalled();

    await refreshDictationSources('laptop');
    expect(dictationAvailable('laptop')).toBe(false);
    read.mockImplementation(async () => ({ subscription: true }));
    applyDictationSourcesChanged('laptop');
    await vi.waitFor(() => expect(dictationAvailable('laptop')).toBe(true));
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
    expect(dictationAvailable('laptop')).toBe(false);
  });
});

describe('composer dictation registry', () => {
  it("routes the chord to that pane's session and forgets it on release", () => {
    const toggle = vi.fn();
    const session = { toggle } as unknown as DictationSession;
    const release = registerComposerDictation('pane-1', session, () => true);
    expect(toggleComposerDictation('pane-1')).toBe(true);
    expect(toggleComposerDictation('pane-2')).toBe(false);
    expect(toggleComposerDictation(null)).toBe(false);
    expect(toggle).toHaveBeenCalledTimes(1);

    // A later session on the same pane is not released by the earlier one.
    const next = { toggle: vi.fn() } as unknown as DictationSession;
    const releaseNext = registerComposerDictation('pane-1', next, () => true);
    release();
    expect(toggleComposerDictation('pane-1')).toBe(true);
    releaseNext();
    expect(toggleComposerDictation('pane-1')).toBe(false);
  });

  it('never starts a session whose mic is not shown', () => {
    const toggle = vi.fn();
    let shown = false;
    const release = registerComposerDictation('pane-1', { toggle } as unknown as DictationSession, () => shown);
    expect(toggleComposerDictation('pane-1')).toBe(false);
    expect(toggle).not.toHaveBeenCalled();
    shown = true;
    expect(toggleComposerDictation('pane-1')).toBe(true);
    expect(toggle).toHaveBeenCalledTimes(1);
    release();
  });
});
