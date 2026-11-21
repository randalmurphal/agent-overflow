import { expect, it, vi } from 'vitest';

// O6: on-time answers commit in one batch per catalog, so a refresh
// reconciles the thread list once however many computers answered. The test
// setup has already loaded the thread store, so the counted module graph is
// loaded fresh here.
it('reconciles the thread list once for an on-time refresh of three listed computers', async () => {
  vi.resetModules();
  const reconcile = vi.fn();
  vi.doMock('./eventsThreadRows', async (importOriginal) => {
    const original = await importOriginal<typeof import('./eventsThreadRows')>();
    reconcile.mockImplementation(original.reconcileThreadRows);
    return { ...original, reconcileThreadRows: reconcile };
  });
  const { setBindingMock } = await import('../../test/mocks/bindings-app');
  const { resetStagedBackends, stageBackend } = await import('../../test/helpers/backends');
  const { makeThread } = await import('../../test/helpers/chat');
  const { HOME_BACKEND, takePinnedBackend } = await import('../transport/backends');
  const { getThreads } = await import('./threads.svelte');
  const { catalogTreeListed, loadThreadCatalogs } = await import('./threadCatalogs');
  try {
    stageBackend({ id: 'gpu' });
    stageBackend({ id: 'laptop' });
    setBindingMock('ListThreadGroups', async () => []);
    setBindingMock('ListThreads', async () => [makeThread({ id: `${takePinnedBackend() || 'home'}-thread` })]);
    await loadThreadCatalogs();
    await vi.waitFor(() => expect([HOME_BACKEND, 'gpu', 'laptop'].map(catalogTreeListed)).toEqual([true, true, true]));
    reconcile.mockClear();

    setBindingMock('ListThreads', async () => [makeThread({ id: `${takePinnedBackend() || 'home'}-refreshed` })]);
    await loadThreadCatalogs();
    await vi.waitFor(() => expect(getThreads().map((thread) => thread.id).sort())
      .toEqual(['gpu-refreshed', 'home-refreshed', 'laptop-refreshed']));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(reconcile).toHaveBeenCalledTimes(1);
  } finally {
    resetStagedBackends();
    vi.doUnmock('./eventsThreadRows');
    vi.resetModules();
  }
});
