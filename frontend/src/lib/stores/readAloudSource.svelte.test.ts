// Whether each computer reads aloud in OpenAI voices: asked only with
// permission, newest answer wins, forgotten on detach, re-read when its
// switch changes or this window comes back, and the clip fetch pinned to
// that computer.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { hasScope } from '../transport/scopes';
import { __attachBackendForTest, __resetBackendsForTest, detachBackend, getPinnedBackend } from '../transport/backends';
import { setupEventListeners } from './events';
import {
  applyReadAloudSourceChanged,
  installReadAloudSourceRefresh,
  readAloudClipFetcher,
  readAloudSourceAvailable,
  readAloudSourceVoiceNames,
  readAloudSourceVoices,
  refreshReadAloudSource,
  resetReadAloudSourcesForTest,
} from './readAloudSource.svelte';

vi.mock('../transport/scopes', async (original) => ({ ...await original<object>(), hasScope: vi.fn(() => true) }));

const LAPTOP_ID = '99999999-8888-4777-8666-555555555555';
const VOICES = [{ id: 'marin', name: 'Marin' }, { id: 'cedar', name: 'Cedar' }];

const fakeClient = {
  callByID: async () => null, callByName: async () => null, subscribe: () => () => undefined,
  installStepUpProver: () => undefined, setLease: () => undefined, setWatchedThreads: () => undefined,
  getStatus: () => ({ status: 'connected', nextAttemptAt: null }), onStatusChange: () => () => undefined,
  getHello: () => null, onHelloChange: () => () => undefined, onReplay: () => () => undefined, close: () => undefined,
};

function attachLaptop(): void {
  __attachBackendForTest({
    id: 'laptop', backendId: LAPTOP_ID, name: 'Laptop',
    wsUrl: 'ws://localhost:3000/ws/backend/laptop', bootstrapUrl: '/bootstrap/laptop.json',
  }, fakeClient as never);
}

const source = (available: boolean) => ({ available, voices: VOICES });

/** ReadAloudSource answers in the order the test releases them. */
function heldAnswers() {
  const answers: Array<(available: boolean) => void> = [];
  setBindingMock('ReadAloudSource', () => new Promise((resolve) => {
    answers.push((available) => resolve(source(available)));
  }));
  return answers;
}

beforeEach(() => {
  resetBindingMocks();
  resetReadAloudSourcesForTest();
  attachLaptop();
});

afterEach(() => {
  vi.mocked(hasScope).mockReset().mockReturnValue(true);
  __resetBackendsForTest();
  vi.unstubAllGlobals();
});

describe('read-aloud sources', () => {
  it('asks the computer it names, and offers voices only while available', async () => {
    const targets: Array<string | null> = [];
    const read = setBindingMock('ReadAloudSource', async () => {
      targets.push(getPinnedBackend());
      return source(false);
    });
    expect(readAloudSourceAvailable('laptop')).toBe(false);
    expect(readAloudSourceVoiceNames('laptop')).toEqual([]);
    await refreshReadAloudSource('laptop');
    expect(targets).toEqual(['laptop']);
    expect(readAloudSourceAvailable('laptop')).toBe(false);
    expect(readAloudSourceVoices('laptop')).toEqual([]);
    expect(readAloudSourceVoiceNames('laptop')).toEqual(VOICES);

    read.mockImplementation(async () => source(true));
    await refreshReadAloudSource('laptop');
    expect(readAloudSourceAvailable('laptop')).toBe(true);
    expect(readAloudSourceVoices('laptop')).toEqual(VOICES);
  });

  it('lets the newest read win over an older one that answers later', async () => {
    const answers = heldAnswers();
    const first = refreshReadAloudSource('laptop');
    const second = refreshReadAloudSource('laptop');
    await vi.waitFor(() => expect(answers).toHaveLength(2));
    answers[1](true);
    await second;
    answers[0](false);
    await first;
    expect(readAloudSourceAvailable('laptop')).toBe(true);
  });

  it('asks nothing without permission to operate threads there, and forgets the old answer', async () => {
    setBindingMock('ReadAloudSource', async () => source(true));
    await refreshReadAloudSource('laptop');
    expect(readAloudSourceAvailable('laptop')).toBe(true);
    const read = setBindingMock('ReadAloudSource', async () => source(true));
    vi.mocked(hasScope).mockImplementation((scope) => scope !== 'threads:operate');
    await refreshReadAloudSource('laptop');
    expect(read).not.toHaveBeenCalled();
    expect(readAloudSourceAvailable('laptop')).toBe(false);
  });

  it('forgets a detached computer and drops its read in flight', async () => {
    setBindingMock('ReadAloudSource', async () => source(true));
    await refreshReadAloudSource('laptop');
    const answers = heldAnswers();
    const read = refreshReadAloudSource('laptop');
    await vi.waitFor(() => expect(answers).toHaveLength(1));
    detachBackend('laptop');
    expect(readAloudSourceAvailable('laptop')).toBe(false);
    answers[0](true);
    await read;
    expect(readAloudSourceAvailable('laptop')).toBe(false);
    // Forgotten: a later change there asks nothing.
    applyReadAloudSourceChanged('laptop');
    expect(answers).toHaveLength(1);
  });

  it('re-reads on a settings change naming the switch, only for a computer someone asked about', async () => {
    setBindingMock('GetSettings', async () => null);
    const cleanup = setupEventListeners();
    try {
      const read = setBindingMock('ReadAloudSource', async () => source(false));
      emitWailsEvent('settings:updated', { keys: ['readAloudAPIEnabled'] }, LAPTOP_ID);
      expect(read).not.toHaveBeenCalled();

      await refreshReadAloudSource('laptop');
      read.mockImplementation(async () => source(true));
      emitWailsEvent('settings:updated', { keys: ['dictationAPIEnabled'] }, LAPTOP_ID);
      expect(read).toHaveBeenCalledTimes(1);
      emitWailsEvent('settings:updated', { keys: ['readAloudAPIEnabled'] }, LAPTOP_ID);
      await vi.waitFor(() => expect(readAloudSourceAvailable('laptop')).toBe(true));
      expect(read).toHaveBeenCalledTimes(2);
    } finally {
      cleanup();
    }
  });

  it('re-reads every computer asked about when the window comes back, and stops when removed', async () => {
    const read = setBindingMock('ReadAloudSource', async () => source(false));
    const remove = installReadAloudSourceRefresh();
    window.dispatchEvent(new Event('focus'));
    expect(read).not.toHaveBeenCalled();

    await refreshReadAloudSource('laptop');
    read.mockImplementation(async () => source(true));
    window.dispatchEvent(new Event('focus'));
    await vi.waitFor(() => expect(readAloudSourceAvailable('laptop')).toBe(true));
    expect(read).toHaveBeenCalledTimes(2);

    const hidden = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    document.dispatchEvent(new Event('visibilitychange'));
    expect(read).toHaveBeenCalledTimes(2);
    hidden.mockRestore();

    remove();
    window.dispatchEvent(new Event('focus'));
    document.dispatchEvent(new Event('visibilitychange'));
    expect(read).toHaveBeenCalledTimes(2);
  });
});

describe('readAloudClipFetcher', () => {
  it('admits the sentence on its computer, then spends the ticket once and answers the audio', async () => {
    const targets: Array<string | null> = [];
    const mint = setBindingMock('MintReadAloudTicket', async () => {
      targets.push(getPinnedBackend());
      return '/attachments/speech/s1?ticket=t1';
    });
    const urls: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      urls.push(String(input));
      return new Response('RIFF clip', { status: 200, headers: { 'Content-Type': 'audio/wav' } });
    }));
    const blob = await readAloudClipFetcher('laptop')('One sentence.', 'marin', new AbortController().signal);
    expect(mint).toHaveBeenCalledWith('One sentence.', 'marin');
    expect(targets).toEqual(['laptop']);
    expect(urls).toHaveLength(1);
    expect(urls[0]).toMatch(/\/attachments\/speech\/s1\?ticket=t1$/);
    expect(await blob.text()).toBe('RIFF clip');
  });

  it('fails with the route’s own reason, and with the computer’s refusal of the sentence', async () => {
    setBindingMock('MintReadAloudTicket', async () => '/attachments/speech/s1?ticket=t1');
    vi.stubGlobal('fetch', vi.fn(async () => new Response('OpenAI took longer than 45 seconds to read the sentence aloud', { status: 504 })));
    const fetchClip = readAloudClipFetcher('laptop');
    await expect(fetchClip('One.', 'marin', new AbortController().signal))
      .rejects.toThrow(/^OpenAI took longer than 45 seconds to read the sentence aloud$/);

    vi.stubGlobal('fetch', vi.fn(async () => new Response('this sentence was already read aloud or has expired', { status: 404 })));
    await expect(fetchClip('One.', 'marin', new AbortController().signal))
      .rejects.toThrow(/^this sentence was already read aloud or has expired$/);

    const refusal = new Error('a sentence to read aloud is at most 1000 characters');
    setBindingMock('MintReadAloudTicket', async () => { throw refusal; });
    await expect(fetchClip('One.', 'marin', new AbortController().signal)).rejects.toBe(refusal);
  });

  it('spends no ticket for a reading stopped while the sentence was admitted', async () => {
    const controller = new AbortController();
    setBindingMock('MintReadAloudTicket', async () => {
      controller.abort();
      return '/attachments/speech/s1?ticket=t1';
    });
    const fetch = vi.fn();
    vi.stubGlobal('fetch', fetch);
    await expect(readAloudClipFetcher('laptop')('One.', 'marin', controller.signal)).rejects.toThrow();
    expect(fetch).not.toHaveBeenCalled();
  });
});
