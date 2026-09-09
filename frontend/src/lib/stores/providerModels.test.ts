import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { buildPane, makeThread } from '../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { resetPanesForTest } from './panes.svelte';
import { getToasts, removeToast } from './toast.svelte';
import { ensureProviderModels, getProviderModels, refreshProviderModels, resetProviderModelsForTest } from './providerModels.svelte';
import type { ModelInfo } from '../types/settings';

const available: ModelInfo = {
  slug: 'gpt-known', name: 'Known', provider: 'codex',
  capabilities: ['fast_mode'],
  reasoningEfforts: [{ slug: 'high', label: 'High', default: true }],
  contextWindows: [],
};

describe('catalog refresh advisories', () => {
  beforeEach(() => {
    resetPanesForTest();
    resetBindingMocks();
    resetProviderModelsForTest();
    for (const toast of getToasts()) removeToast(toast.id);
  });
  afterEach(() => vi.restoreAllMocks());

  it('keeps a usable catalog during expiry and failed refresh', async () => {
    const now = vi.spyOn(Date, 'now').mockReturnValue(1000);
    setBindingMock('GetModelsForProvider', async () => [available]);
    await ensureProviderModels('codex');
    let reject!: (error: Error) => void;
    const request = new Promise<ModelInfo[]>((_, fail) => { reject = fail; });
    const refresh = setBindingMock('GetModelsForProvider', () => request);
    now.mockReturnValue(1_000_000);
    expect(await ensureProviderModels('codex')).toEqual([available]);
    expect(refresh).toHaveBeenCalledOnce();
    expect(await ensureProviderModels('codex')).toEqual([available]);
    expect(refresh).toHaveBeenCalledOnce();
    reject(new Error('offline'));
    await request.catch(() => {});
    await Promise.resolve();
    expect(getProviderModels('codex')).toEqual([available]);
    expect(getToasts()).toHaveLength(0);
  });

  it('warns once for a contradictory selection and never changes it', async () => {
    setBindingMock('GetModelsForProvider', async () => [available]);
    await ensureProviderModels('codex');
    const pane = await buildPane(makeThread({ provider: 'codex', model: available.slug, reasoningEffort: 'high', fastMode: true }));
    setBindingMock('GetModelsForProvider', async () => [{ ...available, reasoningEfforts: [], capabilities: [] }]);
    await refreshProviderModels('codex');
    expect(getToasts()).toHaveLength(1);
    expect(getToasts()[0].message).toContain('high effort and fast mode');
    expect(pane.thread?.reasoningEffort).toBe('high');
    expect(pane.thread?.fastMode).toBe(true);
    await refreshProviderModels('codex');
    expect(getToasts()).toHaveLength(1);
    setBindingMock('GetModelsForProvider', async () => [available]);
    await refreshProviderModels('codex');
    setBindingMock('GetModelsForProvider', async () => []);
    await refreshProviderModels('codex');
    expect(getToasts()).toHaveLength(2);
    expect(getToasts()[1].message).toContain('no longer lists gpt-known');
  });

  it('does not warn from a superseded refresh', async () => {
    setBindingMock('GetModelsForProvider', async () => [available]);
    await ensureProviderModels('codex');
    await buildPane(makeThread({ provider: 'codex', model: available.slug, reasoningEffort: 'high' }));
    let resolve!: (models: ModelInfo[]) => void;
    setBindingMock('GetModelsForProvider', () => new Promise<ModelInfo[]>((done) => { resolve = done; }));
    const old = refreshProviderModels('codex');
    setBindingMock('GetModelsForProvider', async () => [available]);
    await refreshProviderModels('codex');
    resolve([]);
    await old;
    expect(getToasts()).toHaveLength(0);
    expect(getProviderModels('codex')).toEqual([available]);
  });
});
