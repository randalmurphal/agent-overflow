import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { buildPane, makeThread } from '../../test/helpers/chat';
import { modelCatalog } from '../../test/helpers/modelCatalog';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { resetPanesForTest } from './panes.svelte';
import { getToasts, removeToast } from './toast.svelte';
import { ensureProviderModels, getProviderModels, invalidateProviderModels, refreshProviderModels, resetProviderModelsForTest } from './providerModels.svelte';
import type { ModelInfo } from '../types/settings';

const available: ModelInfo = {
  slug: 'gpt-known', name: 'Known', provider: 'codex',
  capabilities: ['fast_mode'],
  reasoningEfforts: [{ slug: 'high', label: 'High', default: true }],
  contextWindows: [],
};

const fable: ModelInfo = { slug: 'claude-fable-5-1', name: 'Fable 5.1', provider: 'claude', contextWindows: [] };
const opus: ModelInfo = { slug: 'claude-opus-5', name: 'Opus 5', provider: 'claude', contextWindows: [] };

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
    setBindingMock('GetModelsForProvider', async () => modelCatalog([available], 'live'));
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

  // The shipped list is what the backend answers before the account probe
  // lands. It cannot know the account's models, so it never contradicts one.
  it('never warns from a shipped catalog, even on a cold start', async () => {
    setBindingMock('GetModelsForProvider', async () => modelCatalog([opus], 'shipped'));
    await buildPane(makeThread({ provider: 'claude', model: fable.slug }));
    await ensureProviderModels('claude');
    expect(getProviderModels('claude')).toEqual([opus]);
    expect(getToasts()).toHaveLength(0);
    await refreshProviderModels('claude');
    expect(getToasts()).toHaveLength(0);
    setBindingMock('GetModelsForProvider', async () => modelCatalog([opus, fable]));
    await refreshProviderModels('claude');
    expect(getProviderModels('claude')).toEqual([opus, fable]);
    expect(getToasts()).toHaveLength(0);
  });

  it('treats the first authoritative catalog as the baseline, not a contradiction', async () => {
    await buildPane(makeThread({ provider: 'claude', model: fable.slug }));
    setBindingMock('GetModelsForProvider', async () => modelCatalog([opus]));
    await ensureProviderModels('claude');
    expect(getToasts()).toHaveLength(0);
  });

  it('warns once when an authoritative refresh withdraws a selection and never changes it', async () => {
    setBindingMock('GetModelsForProvider', async () => modelCatalog([available], 'live'));
    await ensureProviderModels('codex');
    const pane = await buildPane(makeThread({ provider: 'codex', model: available.slug, reasoningEffort: 'high', fastMode: true }));
    setBindingMock('GetModelsForProvider', async () => modelCatalog([{ ...available, reasoningEfforts: [], capabilities: [] }], 'live'));
    await refreshProviderModels('codex');
    expect(getToasts()).toHaveLength(1);
    expect(getToasts()[0].message).toContain('high effort and fast mode');
    expect(pane.thread?.reasoningEffort).toBe('high');
    expect(pane.thread?.fastMode).toBe(true);
    await refreshProviderModels('codex');
    expect(getToasts()).toHaveLength(1);
    setBindingMock('GetModelsForProvider', async () => modelCatalog([available], 'live'));
    await refreshProviderModels('codex');
    expect(getToasts()).toHaveLength(1);
    setBindingMock('GetModelsForProvider', async () => modelCatalog([], 'live'));
    await refreshProviderModels('codex');
    expect(getToasts()).toHaveLength(2);
    expect(getToasts()[1].message).toContain('no longer lists gpt-known');
  });

  it('names only the parts a refresh newly withdrew', async () => {
    setBindingMock('GetModelsForProvider', async () => modelCatalog([{ ...available, capabilities: [] }], 'live'));
    await ensureProviderModels('codex');
    await buildPane(makeThread({ provider: 'codex', model: available.slug, reasoningEffort: 'high', fastMode: true }));
    setBindingMock('GetModelsForProvider', async () => modelCatalog([{ ...available, reasoningEfforts: [], capabilities: [] }], 'live'));
    await refreshProviderModels('codex');
    expect(getToasts()).toHaveLength(1);
    expect(getToasts()[0].message).toContain('no longer lists high effort for gpt-known');
  });

  it('compares against the last authoritative catalog across shipped answers and invalidation', async () => {
    setBindingMock('GetModelsForProvider', async () => modelCatalog([opus, fable]));
    await ensureProviderModels('claude');
    await buildPane(makeThread({ provider: 'claude', model: fable.slug }));
    setBindingMock('GetModelsForProvider', async () => modelCatalog([opus], 'shipped'));
    await refreshProviderModels('claude');
    expect(getProviderModels('claude')).toEqual([opus]);
    expect(getToasts()).toHaveLength(0);
    invalidateProviderModels('claude');
    setBindingMock('GetModelsForProvider', async () => modelCatalog([opus]));
    await ensureProviderModels('claude');
    expect(getToasts()).toHaveLength(1);
    expect(getToasts()[0].message).toContain('no longer lists claude-fable-5-1');
  });

  it('warns once for several panes on the same selection', async () => {
    setBindingMock('GetModelsForProvider', async () => modelCatalog([opus, fable]));
    await ensureProviderModels('claude');
    const thread = makeThread({ provider: 'claude', model: fable.slug });
    await buildPane(thread);
    await buildPane(makeThread({ provider: 'claude', model: fable.slug }));
    setBindingMock('GetModelsForProvider', async () => modelCatalog([opus]));
    await refreshProviderModels('claude');
    expect(getToasts()).toHaveLength(1);
  });

  it('does not warn from a superseded refresh', async () => {
    setBindingMock('GetModelsForProvider', async () => modelCatalog([available], 'live'));
    await ensureProviderModels('codex');
    await buildPane(makeThread({ provider: 'codex', model: available.slug, reasoningEffort: 'high' }));
    let resolve!: (catalog: unknown) => void;
    setBindingMock('GetModelsForProvider', () => new Promise<unknown>((done) => { resolve = done; }));
    const old = refreshProviderModels('codex');
    setBindingMock('GetModelsForProvider', async () => modelCatalog([available], 'live'));
    await refreshProviderModels('codex');
    resolve(modelCatalog([], 'live'));
    await old;
    expect(getToasts()).toHaveLength(0);
    expect(getProviderModels('codex')).toEqual([available]);
  });
});
