import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  reportBrowserPaneRect,
  resetBrowserCompanionForTest,
  setBrowserPaneMountForTest,
} from './browserCompanion.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

const THREAD = 'thread-browser';

function rect(x: number) {
  return {
    x, y: 20, width: 800, height: 600,
    clipX: x, clipY: 20, clipWidth: 800, clipHeight: 600,
    viewportWidth: 1600, viewportHeight: 900, devicePixelRatio: 1.25,
    visible: true, background: '#000000',
  };
}

describe('browser pane geometry to the window host', () => {
  let posted: string[];
  let backend: number[];

  beforeEach(() => {
    resetBrowserCompanionForTest();
    posted = [];
    backend = [];
    setBindingMock('BrowserCompanionPaneRect', async (_mount: string, r: { x: number }) => {
      backend.push(r.x);
    });
    setBrowserPaneMountForTest(THREAD, 'mount-1');
  });

  afterEach(() => {
    resetBrowserCompanionForTest();
    resetBindingMocks();
    vi.unstubAllGlobals();
  });

  function stubHost(advertised: boolean): void {
    vi.stubGlobal('chrome', { webview: { postMessage: (message: string) => posted.push(message) } });
    if (advertised) vi.stubGlobal('__aoPaneGeometryBridge', 1);
  }

  it('posts nothing to a host that does not advertise the bridge', () => {
    stubHost(false);
    reportBrowserPaneRect(THREAD, rect(1));
    expect(posted).toEqual([]);
    expect(backend).toEqual([1]);
  });

  it('posts each changed report numbered per mount, and still reports to the backend', async () => {
    stubHost(true);
    reportBrowserPaneRect(THREAD, rect(1));
    reportBrowserPaneRect(THREAD, rect(1));
    reportBrowserPaneRect(THREAD, rect(2));
    expect(posted).toHaveLength(2);
    const reports = posted.map((message) => {
      expect(message.startsWith('ao:pane-geometry:')).toBe(true);
      return JSON.parse(message.slice('ao:pane-geometry:'.length));
    });
    expect(reports.map((r) => [r.mount, r.seq, r.x])).toEqual([
      ['mount-1', 1, 1],
      ['mount-1', 2, 2],
    ]);
    expect(reports[0]).toMatchObject({ devicePixelRatio: 1.25, visible: true, background: '#000000', clipWidth: 800 });
    await Promise.resolve();
    await Promise.resolve();
    expect(backend[0]).toBe(1);
  });

  it('reports nothing for a thread with no mount', () => {
    stubHost(true);
    reportBrowserPaneRect('other-thread', rect(1));
    expect(posted).toEqual([]);
    expect(backend).toEqual([]);
  });
});
