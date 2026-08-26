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

  // Row-reuse tripwire: the projection derived re-runs on every data
  // identity change, and the keyed each writes each row into a per-key
  // signal. Unchanged rows must keep their previous row OBJECT, or every
  // mounted row's wrapper effects and snippet content re-fire on every
  // streamed chunk — O(window) per reveal tick (2026-08-26, the 165Hz
  // frame-drop attribution). `onRowRender` runs from a template expression
  // inside the row snippet, so a count is a snippet-content re-render.
  describe('row identity reuse across projection passes', () => {
    it('appending a row re-renders only the new row', async () => {
      const host = document.createElement('div');
      document.body.appendChild(host);
      const counts = new Map<string, number>();
      const app = mount(TimelineVirtualizerHarness, {
        target: host,
        props: {
          initialRows: makeRows(3),
          renderAll: true,
          onRowRender: (id: string) => counts.set(id, (counts.get(id) ?? 0) + 1),
        },
      });
      try {
        expect(counts.size).toBe(3);
        counts.clear();
        app.setRows([...app.getRows(), { id: 'row-new', heightPx: 100, label: 'New' }]);
        await tick();
        expect(counts.get('row-new')).toBe(1);
        expect([...counts.keys()]).toEqual(['row-new']);
      } finally {
        unmount(app);
        host.remove();
      }
    });

    it('replacing one row item re-renders exactly that row', async () => {
      const host = document.createElement('div');
      document.body.appendChild(host);
      const counts = new Map<string, number>();
      const app = mount(TimelineVirtualizerHarness, {
        target: host,
        props: {
          initialRows: makeRows(3),
          renderAll: true,
          onRowRender: (id: string) => counts.set(id, (counts.get(id) ?? 0) + 1),
        },
      });
      try {
        counts.clear();
        app.resizeRow('row-1', 140);
        await tick();
        expect([...counts.keys()]).toEqual(['row-1']);
        expect(counts.get('row-1')).toBe(1);
      } finally {
        unmount(app);
        host.remove();
      }
    });
  });
});
