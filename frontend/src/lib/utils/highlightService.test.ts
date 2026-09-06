import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock, resetBindingMocks } from '../../test/mocks/bindings-app';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { backendById, detachBackend, takePinnedBackend } from '../transport/backends';
import { __resetBackendIdentityForTest, setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { DisconnectedError, type TransportHello } from '../transport/wsClient';
import { assertHighlightSource, highlightMetadata, requireHighlightSchema, resetHighlightServiceForTest, withHighlightBackend, withHighlightService } from './highlightService';
import { getCachedBlockSpans, requestBlockSpans, resetCodeSpanCacheForTest } from '../components/chat/markdown/codeSpanCache';
import { getToasts } from '../stores/toast.svelte';
import { applyHighlightSeed } from '../stores/eventsHighlight';
import { contentKey } from './fnv1a';

vi.mock('../native/platform', async (importOriginal) => ({
  ...await importOriginal<typeof import('../native/platform')>(),
  isNativeShell: () => true,
}));

const versions = new Map<string, string>();
let schemaReads: string[];
let computeReads: string[];

beforeEach(() => {
  resetStagedBackends();
  __resetBackendIdentityForTest();
  resetHighlightServiceForTest();
  resetCodeSpanCacheForTest();
  resetBindingMocks();
  detachBackend('');
  versions.clear();
  schemaReads = [];
  computeReads = [];
  setBindingMock('HighlightSchemaVersion', async () => {
    const key = takePinnedBackend()!;
    schemaReads.push(key);
    if (backendById(key)?.status.status !== 'connected') throw new DisconnectedError();
    return versions.get(key) ?? 'hv-one';
  });
  setBindingMock('HighlightClassNames', async () => { takePinnedBackend(); return ['none', 'keyword']; });
});
afterEach(() => { resetHighlightServiceForTest(); resetStagedBackends(); __resetBackendIdentityForTest(); });

function add(id: string, status: 'connected' | 'reconnecting' = 'connected') {
  return stageBackend({ id, backendId: `${id}-uuid`, status });
}
function compute() {
  const key = takePinnedBackend()!;
  computeReads.push(key);
  if (backendById(key)?.status.status !== 'connected') return Promise.reject(new DisconnectedError());
  return Promise.resolve(key);
}

describe('highlight rendering service', () => {
  it('uses the paired computer with no HOME and memoizes one schema/table pair', async () => {
    add('mac');
    expect(await withHighlightService(compute)).toBe('mac');
    expect(await withHighlightService(compute)).toBe('mac');
    expect(schemaReads).toEqual(['mac']);
  });

  it('starts with a healthy host even when the first attachment is offline', async () => {
    add('mac', 'reconnecting');
    add('gpu');
    expect(await withHighlightService(compute)).toBe('gpu');
    expect(schemaReads).toEqual(['gpu']);
  });

  it('fails over after disconnection and removal without changing the canonical schema', async () => {
    const mac = add('mac');
    add('gpu');
    expect(await withHighlightService(compute)).toBe('mac');
    mac.setStatus('reconnecting');
    expect(await withHighlightService(compute)).toBe('gpu');
    detachBackend('mac');
    expect(await withHighlightService(compute)).toBe('gpu');
    expect(schemaReads).toEqual(['mac', 'gpu']);
  });

  it('refuses incompatible origin spans and never sends code to that host', async () => {
    add('mac');
    add('gpu');
    versions.set('gpu', 'hv-two');
    await highlightMetadata();
    await expect(requireHighlightSchema('gpu')).rejects.toThrow('incompatible');
    detachBackend('mac');
    await expect(withHighlightService(compute)).rejects.toThrow('incompatible');
    expect(computeReads).toEqual([]);
    expect(schemaReads).toEqual(['mac', 'gpu']);
  });

  it('revalidates on reconnect even when the database generation has not changed', async () => {
    const mac = add('mac');
    await withHighlightService(compute);
    versions.set('mac', 'hv-two');
    mac.setHello({ backendId: 'mac-uuid' } as TransportHello);
    await expect(withHighlightService(compute)).rejects.toThrow('incompatible');
    expect(schemaReads).toEqual(['mac', 'mac']);
    expect(computeReads).toEqual(['mac']);
  });

  it('drops a response when the host restarts between the schema check and compute completion', async () => {
    const mac = add('mac');
    await highlightMetadata();
    await expect(withHighlightBackend('mac', async () => {
      mac.setHello({ backendId: 'mac-uuid' } as TransportHello);
      return 'stale';
    })).rejects.toBeInstanceOf(DisconnectedError);
  });

  it('carries the validated connection stamp across asynchronous caller continuations', async () => {
    const mac = add('mac');
    const validated = await requireHighlightSchema('mac');
    mac.setHello({ backendId: 'mac-uuid' } as TransportHello);
    expect(() => assertHighlightSource(validated)).toThrow(DisconnectedError);
  });

  it('refuses a mismatched live seed before it can enter the global content cache', async () => {
    add('mac');
    add('gpu');
    versions.set('gpu', 'hv-two');
    await highlightMetadata();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      applyHighlightSeed({
        threadId: 't1', itemId: 'i1', lang: 'sh', lineHashes: [],
        contentKey: contentKey('echo'), final: true, lines: [{ r: [4, 1] }],
      }, { backendId: 'gpu' });
      await vi.waitFor(() => expect(warn).toHaveBeenCalled());
      expect(getCachedBlockSpans('sh', 'echo')).toBeNull();
    } finally {
      warn.mockRestore();
    }
  });

  it('invalidates cached metadata on a replica generation change', async () => {
    add('mac');
    setBackendIdentityFromBootstrap('mac-uuid', 'first', 'Mac', 'mac');
    await withHighlightService(compute);
    versions.set('mac', 'hv-two');
    setBackendIdentityFromBootstrap('mac-uuid', 'second', 'Mac', 'mac');
    await expect(withHighlightService(compute)).rejects.toThrow('incompatible');
    expect(schemaReads).toEqual(['mac', 'mac']);
  });

  it('keeps connection failures quiet and permits a later code request to recover', async () => {
    const mac = add('mac', 'reconnecting');
    setBindingMock('HighlightCode', async () => { takePinnedBackend(); return { lines: [{ r: [1, 1] }] }; });
    const before = getToasts().length;
    expect(await requestBlockSpans('sh', 'x')).toBeNull();
    expect(getToasts().length).toBe(before);
    mac.setStatus('connected');
    expect(await requestBlockSpans('sh', 'x')).toEqual([{ r: [1, 1] }]);
  });
});
