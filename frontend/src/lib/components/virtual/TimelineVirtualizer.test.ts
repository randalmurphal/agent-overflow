// happy-dom sanity for the renderAll seam: zero geometry means no RO
// deliveries and no viewport, so only renderAll mode mounts rows — the
// same behavior the unit project relied on with virtua's ssrCount shim.
// Everything geometric runs in timelineVirtualizer.browser.test.ts.

import { describe, expect, it } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import TimelineVirtualizerHarness, { type HarnessRow } from './TimelineVirtualizerHarness.svelte';

function makeRows(count: number): HarnessRow[] {
  return Array.from({ length: count }, (_, i) => ({
    id: `row-${i}`,
    heightPx: 100,
    label: `Row ${i}`,
  }));
}

describe('TimelineVirtualizer under happy-dom', () => {
  it('renderAll mounts every row without geometry', () => {
    const host = document.createElement('div');
    document.body.appendChild(host);
    const app = mount(TimelineVirtualizerHarness, {
      target: host,
      props: { initialRows: makeRows(12), renderAll: true },
    });
    try {
      expect(host.querySelectorAll('[data-row-index]').length).toBe(12);
    } finally {
      unmount(app);
      host.remove();
    }
  });

  it('windowed mode mounts nothing until a viewport is measured', () => {
    const host = document.createElement('div');
    document.body.appendChild(host);
    const app = mount(TimelineVirtualizerHarness, {
      target: host,
      props: { initialRows: makeRows(12) },
    });
    try {
      expect(host.querySelectorAll('[data-row-index]').length).toBe(0);
    } finally {
      unmount(app);
      host.remove();
    }
  });

  it('renderAll tracks data growth (streaming append in unit tests)', async () => {
    const host = document.createElement('div');
    document.body.appendChild(host);
    const app = mount(TimelineVirtualizerHarness, {
      target: host,
      props: { initialRows: makeRows(3), renderAll: true },
    });
    try {
      app.setRows([...app.getRows(), { id: 'row-new', heightPx: 100, label: 'New' }]);
      await tick();
      expect(host.querySelectorAll('[data-row-index]').length).toBe(4);
    } finally {
      unmount(app);
      host.remove();
    }
  });

  it('rejects duplicate row keys at the component boundary', () => {
    const host = document.createElement('div');
    document.body.appendChild(host);
    const duplicate = { id: 'same', heightPx: 100, label: 'Duplicate' };
    try {
      expect(() =>
        mount(TimelineVirtualizerHarness, {
          target: host,
          props: { initialRows: [duplicate, { ...duplicate }], renderAll: true },
        }),
      ).toThrow(/unique key/);
    } finally {
      host.remove();
    }
  });
});
