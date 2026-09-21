import { expect, it } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import TimelineVirtualizerHarness from './TimelineVirtualizerHarness.svelte';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { captureResizeObserverLoopErrors } from '../../../test/helpers/resizeObserverLoopErrors';

it('commits fresh geometry for retained rows and expanded mount windows before completing', async () => {
  const host = document.createElement('div');
  document.body.append(host);
  const errors = captureResizeObserverLoopErrors();
  const app = mount(TimelineVirtualizerHarness, {
    target: host,
    props: { initialRows: Array.from({ length: 60 }, (_, i) => ({ id: `row-${i}`, heightPx: 100, label: `Row ${i}` })),
      viewportPx: 600, bufferSize: 400 },
  });
  try {
    await waitFor(() => Boolean(app.handle()), 'virtualizer handle');
    const list = app.handle()!;
    await list.measureMountedRows(new AbortController().signal);
    const before = list.sizeAt(59);
    await raf();
    app.resizeRow('row-59', 20);
    await tick();
    await list.measureMountedRows(new AbortController().signal);
    expect(list.sizeAt(59)).toBe(20);
    expect(before).toBe(100);
    // Unchanged rows still produce a fresh measurement and complete.
    await list.measureMountedRows(new AbortController().signal);
    await raf();
    expect(errors.messages).toEqual([]);
  } finally {
    errors.stop();
    await unmount(app);
    host.remove();
  }
});

it('cancels a hidden measurement and completes an overlapping replacement and unmount', async () => {
  const host = document.createElement('div');
  document.body.append(host);
  const app = mount(TimelineVirtualizerHarness, {
    target: host,
    props: { initialRows: [{ id: 'row', heightPx: 100, label: 'row' }], viewportPx: 600, bufferSize: 400 },
  });
  let mounted = true;
  try {
    await waitFor(() => Boolean(app.handle()), 'virtualizer handle');
    const list = app.handle()!;
    await list.measureMountedRows(new AbortController().signal);
    await raf();
    host.style.display = 'none';
    const abort = new AbortController();
    const cancelled = list.measureMountedRows(abort.signal);
    const next = list.measureMountedRows(new AbortController().signal);
    abort.abort();
    await cancelled;
    host.style.display = '';
    await next;
    await raf();
    const pending = list.measureMountedRows(new AbortController().signal);
    await unmount(app);
    mounted = false;
    await pending;
  } finally {
    if (mounted) await unmount(app);
    host.remove();
  }
});
