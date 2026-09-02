import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { waitFor } from '@testing-library/svelte';
import {
  canUseHostOpenExternalURL,
  devServerLabel,
  externalURLForEventTarget,
  handleExternalURL,
  installExternalLinkDelegate,
  installPreviewLinkActions,
  loopbackDevServerURL,
  safeExternalURL,
} from './externalLinks';
import { buildPathLinkHref } from './pathLinkExtension';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { resetRunMode, setRunMode } from '../../test/runMode';
import { OBSERVE_SCOPES, pairWithScopes, resetToLocalPage } from '../../test/helpers/scopes';
import { __resetScopesForTest } from '../transport/scopes';
import { resetBrowserCompanionForTest } from '../stores/browserCompanion.svelte';
import { noteThread, __resetEntityIndexForTest } from '../transport/entityIndex';

describe('safeExternalURL', () => {
  beforeEach(() => {
    resetRunMode();
  });

  it('accepts absolute http and https URLs', () => {
    expect(safeExternalURL('https://example.com/path?q=1')).toBe('https://example.com/path?q=1');
    expect(safeExternalURL('http://localhost:3000/callback')).toBe('http://localhost:3000/callback');
  });

  it('rejects relative and unsupported URLs', () => {
    expect(safeExternalURL('/local/path')).toBeNull();
    expect(safeExternalURL('#fragment')).toBeNull();
    expect(safeExternalURL('mailto:test@example.com')).toBeNull();
    expect(safeExternalURL('javascript:alert(1)')).toBeNull();
  });

  it('rejects browser-normalized hostless URL shapes', () => {
    expect(safeExternalURL('https:///missing-host')).toBeNull();
    expect(safeExternalURL('http:example.com')).toBeNull();
  });
});

describe('handleExternalURL', () => {
  let originalOpen: typeof window.open;

  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
    originalOpen = window.open;
    window.open = vi.fn();
  });

  afterEach(() => {
    window.open = originalOpen;
    resetRunMode();
  });

  it('routes supported URLs through the OpenExternalURL binding', async () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));

    const handled = await handleExternalURL('https://example.com/docs');

    expect(handled).toBe(true);
    expect(open).toHaveBeenCalledWith('https://example.com/docs');
    expect(window.open).not.toHaveBeenCalled();
  });

  it('does not call the binding for unsupported URLs', async () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));

    const handled = await handleExternalURL('javascript:alert(1)');

    expect(handled).toBe(false);
    expect(open).not.toHaveBeenCalled();
  });

  it('uses browser-native opening in client mode', async () => {
    setRunMode('client');
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));

    const handled = await handleExternalURL('https://example.com/docs');

    expect(handled).toBe(true);
    expect(open).not.toHaveBeenCalled();
    expect(window.open).toHaveBeenCalledWith('https://example.com/docs', '_blank', 'noopener,noreferrer');
  });

});

describe('canUseHostOpenExternalURL', () => {
  afterEach(() => {
    resetRunMode();
  });

  it('is true for local loopback origins', () => {
    resetRunMode();

    expect(canUseHostOpenExternalURL('127.0.0.1')).toBe(true);
    expect(canUseHostOpenExternalURL('localhost')).toBe(true);
    expect(canUseHostOpenExternalURL('::1')).toBe(true);
  });

  it('is false for client mode and LAN browser origins', () => {
    setRunMode('client');
    expect(canUseHostOpenExternalURL('127.0.0.1')).toBe(false);

    resetRunMode();
    expect(canUseHostOpenExternalURL('192.168.1.25')).toBe(false);
  });
});

// The one resolver both entry points share: the click delegate here and the
// right-click menu in components/shared/ExternalLinkContextHost.svelte. They
// must agree, or the menu offers to open something the click path refuses.
describe('externalURLForEventTarget', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  function target(html: string): Element {
    document.body.innerHTML = html;
    return document.body.querySelector('[data-hit]')!;
  }

  it('resolves the enclosing anchor, not just a direct hit', () => {
    const hit = target('<a href="https://example.com/x"><span data-hit>label</span></a>');
    expect(externalURLForEventTarget(hit)).toBe('https://example.com/x');
  });

  it('returns null for path links, relative hrefs, and non-links', () => {
    expect(
      externalURLForEventTarget(
        target(`<a data-hit href="${buildPathLinkHref('src/foo.ts', undefined, undefined, '')}">f</a>`),
      ),
    ).toBeNull();
    expect(externalURLForEventTarget(target('<a data-hit href="/docs">d</a>'))).toBeNull();
    expect(externalURLForEventTarget(target('<a data-hit href="javascript:alert(1)">x</a>'))).toBeNull();
    expect(externalURLForEventTarget(target('<span data-hit>plain</span>'))).toBeNull();
    expect(externalURLForEventTarget(null)).toBeNull();
  });
});

describe('installExternalLinkDelegate', () => {
  let cleanup = () => {};
  let originalOpen: typeof window.open;

  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
    originalOpen = window.open;
    window.open = vi.fn();
    document.body.innerHTML = '';
  });

  afterEach(() => {
    cleanup();
    cleanup = () => {};
    window.open = originalOpen;
    resetRunMode();
    document.body.innerHTML = '';
  });

  it('intercepts http anchors and prevents browser-default navigation', async () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    cleanup = installExternalLinkDelegate();
    const link = document.createElement('a');
    link.href = 'https://example.com/from-anchor';
    document.body.appendChild(link);

    const event = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
    const allowed = link.dispatchEvent(event);

    expect(allowed).toBe(false);
    await waitFor(() => expect(open).toHaveBeenCalledWith('https://example.com/from-anchor'));
  });

  it('intercepts middle-click anchors', async () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    cleanup = installExternalLinkDelegate();
    const link = document.createElement('a');
    link.href = 'https://example.com/from-middle-click';
    document.body.appendChild(link);

    const event = new MouseEvent('auxclick', { bubbles: true, cancelable: true, button: 1 });
    const allowed = link.dispatchEvent(event);

    expect(allowed).toBe(false);
    await waitFor(() => expect(open).toHaveBeenCalledWith('https://example.com/from-middle-click'));
  });

  it('uses browser-native opening when host opening is unavailable', async () => {
    setRunMode('client');
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    cleanup = installExternalLinkDelegate();
    const link = document.createElement('a');
    link.href = 'https://example.com/client-mode';
    document.body.appendChild(link);

    const event = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
    const allowed = link.dispatchEvent(event);

    expect(allowed).toBe(false);
    await waitFor(() =>
      expect(window.open).toHaveBeenCalledWith(
        'https://example.com/client-mode',
        '_blank',
        'noopener,noreferrer',
      ),
    );
    expect(open).not.toHaveBeenCalled();
  });

  it('leaves path-link anchors (agent-overflow: scheme) to the path-link delegate', () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    cleanup = installExternalLinkDelegate();
    const link = document.createElement('a');
    // Use buildPathLinkHref so the nonce baked into the prefix is
    // present — that's what the delegate checks for and what a real
    // anchor mounted by the marked extension would carry.
    link.href = buildPathLinkHref('src/foo.ts', undefined, undefined, '');
    document.body.appendChild(link);

    const event = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
    const allowed = link.dispatchEvent(event);

    expect(allowed).toBe(true);
    expect(open).not.toHaveBeenCalled();
  });
});

describe('loopbackDevServerURL', () => {
  it('accepts loopback dev-server URLs', () => {
    expect(loopbackDevServerURL('http://localhost:5173/')).toBe('http://localhost:5173/');
    expect(loopbackDevServerURL('http://127.0.0.1:3000')).toBe('http://127.0.0.1:3000/');
    expect(loopbackDevServerURL('http://127.0.0.53:9000/')).toBe('http://127.0.0.53:9000/');
    expect(loopbackDevServerURL('https://localhost:5174/app')).toBe('https://localhost:5174/app');
    expect(loopbackDevServerURL('http://[::1]:4321/')).toBe('http://[::1]:4321/');
  });

  it('rejects anything that is not a loopback http(s) URL', () => {
    expect(loopbackDevServerURL(null)).toBeNull();
    expect(loopbackDevServerURL('')).toBeNull();
    expect(loopbackDevServerURL('https://example.com/')).toBeNull();
    expect(loopbackDevServerURL('http://192.168.1.24:5173/')).toBeNull();
    expect(loopbackDevServerURL('http://localhost.example.com/')).toBeNull();
    // A name that merely STARTS with "127." is a resolvable public
    // domain, not a loopback address.
    expect(loopbackDevServerURL('http://127.example.com/')).toBeNull();
    expect(loopbackDevServerURL('ws://localhost:5173/hmr')).toBeNull();
    expect(loopbackDevServerURL('javascript:alert(1)')).toBeNull();
  });

  it('rejects wildcard bind addresses the backend is expected to rewrite', () => {
    expect(loopbackDevServerURL('http://0.0.0.0:5173/')).toBeNull();
  });
});

describe('devServerLabel', () => {
  it('reduces a URL to host:port', () => {
    expect(devServerLabel('http://localhost:5173/some/path')).toBe('localhost:5173');
    expect(devServerLabel('http://localhost/')).toBe('localhost');
    expect(devServerLabel('http://[::1]:4321/')).toBe('[::1]:4321');
  });

  it('falls back to the raw value when it cannot be parsed', () => {
    expect(devServerLabel('not a url')).toBe('not a url');
  });
});

// ---------------------------------------------------------------------------
// The preview and companion-browser branches of the same delegate
// (docs/specs/remote-access.md §7, "What the person sees" and "Link clicks
// in general").
// ---------------------------------------------------------------------------

describe('the delegate’s preview branches', () => {
  let cleanup = () => {};
  let originalOpen: typeof window.open;
  const actions = { open: vi.fn(async () => {}), allow: vi.fn(async () => {}) };

  function previewAnchor(attrs: Record<string, string>): HTMLAnchorElement {
    const link = document.createElement('a');
    link.href = 'http://localhost:5173/app';
    for (const [name, value] of Object.entries(attrs)) link.setAttribute(name, value);
    document.body.appendChild(link);
    return link;
  }

  const leftClick = (el: Element): boolean =>
    el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 }));

  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
    resetToLocalPage();
    originalOpen = window.open;
    window.open = vi.fn();
    document.body.innerHTML = '';
    actions.open.mockClear();
    actions.allow.mockClear();
    installPreviewLinkActions(actions);
    cleanup = installExternalLinkDelegate();
  });

  afterEach(() => {
    cleanup();
    cleanup = () => {};
    installPreviewLinkActions(null);
    window.open = originalOpen;
    resetRunMode();
    resetToLocalPage();
    __resetScopesForTest();
    document.body.innerHTML = '';
  });

  it('opens the preview for a link whose port is shared', () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    const link = previewAnchor({
      'data-preview-state': 'open',
      'data-preview-port': '5173',
      'data-preview-path': '/app?q=1',
      'data-preview-thread': 'thread-1',
    });

    expect(leftClick(link)).toBe(false);
    expect(actions.open).toHaveBeenCalledWith('thread-1', 5173, '/app?q=1');
    // Never the system browser: the href names a listener on the machine
    // the agent is on, not on this one.
    expect(open).not.toHaveBeenCalled();
    expect(window.open).not.toHaveBeenCalled();
  });

  it('swallows the click on a link that is inert, and opens nothing', () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    for (const state of ['blocked', 'no-address']) {
      document.body.innerHTML = '';
      const link = previewAnchor({
        'data-preview-state': state,
        'data-preview-port': '5173',
        'data-preview-path': '/',
        'data-preview-thread': 'thread-1',
      });
      expect(leftClick(link)).toBe(false);
    }
    expect(actions.open).not.toHaveBeenCalled();
    expect(open).not.toHaveBeenCalled();
  });

  it('shares the port from the inline Allow action, naming the backend it belongs to', () => {
    const button = document.createElement('button');
    button.dataset.previewAllow = '5173';
    button.dataset.previewBackend = 'laptop';
    document.body.appendChild(button);

    expect(leftClick(button)).toBe(false);
    expect(actions.allow).toHaveBeenCalledWith('laptop', 5173);
  });

  it('does nothing for a preview anchor when no actions are installed', () => {
    installPreviewLinkActions(null);
    const link = previewAnchor({
      'data-preview-state': 'open',
      'data-preview-port': '5173',
      'data-preview-path': '/',
      'data-preview-thread': 'thread-1',
    });
    expect(leftClick(link)).toBe(false);
    expect(actions.open).not.toHaveBeenCalled();
  });
});

describe('mod+click on an ordinary external link', () => {
  let cleanup = () => {};
  let originalOpen: typeof window.open;

  function linkInThread(): HTMLAnchorElement {
    const host = document.createElement('div');
    host.dataset.threadId = 'thread-1';
    const link = document.createElement('a');
    link.href = 'https://example.com/docs';
    host.appendChild(link);
    document.body.appendChild(host);
    return link;
  }

  const modClick = (el: Element, init: MouseEventInit = {}): boolean =>
    el.dispatchEvent(
      new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, metaKey: true, ...init }),
    );

  // Which of the two plain doors a click takes is the run mode's and the
  // page origin's answer, and both are pinned by their own cases above.
  // This block asserts the OUTCOME instead, for a reason worth knowing:
  // one case above deliberately lets a click navigate, which leaves
  // happy-dom's `window.location` blank for every case after it — so the
  // host door is not deterministic from here.
  const openedExternally = (binding: { mock: { calls: unknown[][] } }, url: string): boolean => {
    const viaHost = binding.mock.calls.some((call) => call[0] === url);
    const browserOpen = window.open as unknown as { mock: { calls: unknown[][] } };
    const viaBrowser = browserOpen.mock.calls.some((call) => call[0] === url);
    return viaHost || viaBrowser;
  };

  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
    resetToLocalPage();
    resetBrowserCompanionForTest();
    __resetEntityIndexForTest();
    originalOpen = window.open;
    window.open = vi.fn();
    document.body.innerHTML = '';
    cleanup = installExternalLinkDelegate();
  });

  afterEach(() => {
    cleanup();
    cleanup = () => {};
    resetBrowserCompanionForTest();
    __resetEntityIndexForTest();
    window.open = originalOpen;
    resetRunMode();
    resetToLocalPage();
    __resetScopesForTest();
    document.body.innerHTML = '';
  });

  it('opens a new companion tab on that address, on host', async () => {
    const systemOpen = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({
      threadId: 'thread-1',
      kind: 'state',
      pages: [{ id: 'page-1' }],
      activePageId: 'page-1',
    })));

    expect(modClick(linkInThread())).toBe(false);
    await waitFor(() => expect(act).toHaveBeenCalledTimes(2));
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'new' });
    expect(act.mock.calls[1][1]).toMatchObject({
      kind: 'navigate',
      pageId: 'page-1',
      address: 'https://example.com/docs',
    });
    expect(systemOpen).not.toHaveBeenCalled();
  });

  it('does not navigate when the new tab was refused', async () => {
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => {
      throw new Error('browser manager unavailable');
    }));

    expect(modClick(linkInThread())).toBe(false);
    await waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    expect(act).toHaveBeenCalledTimes(1);
  });

  it('falls back to the plain behaviour off host, where there is no pane to open', async () => {
    // A paired device holds no `host`: the companion pane is a native view
    // the host process paints, which no remote client renders. Which of
    // the two plain doors that page then uses is the run mode's answer,
    // not this branch's, so the assertion is that the click took the
    // ordinary external path at all.
    await pairWithScopes(OBSERVE_SCOPES);
    const systemOpen = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ threadId: 'thread-1', kind: 'state', pages: [] })));

    expect(modClick(linkInThread())).toBe(false);
    await waitFor(() =>
      expect(openedExternally(systemOpen, 'https://example.com/docs')).toBe(true),
    );
    expect(act).not.toHaveBeenCalled();
  });

  it('falls back to the plain behaviour when the thread runs on another machine', async () => {
    // `host` in hand says nothing about WHERE the pane would open:
    // `browserCompanionAct` routes to the thread's backend, so on a thread
    // attached to another machine the tab would be minted in an engine
    // this window cannot paint, and the person would see nothing happen.
    noteThread('thread-1', 'laptop');
    const systemOpen = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ threadId: 'thread-1', kind: 'state', pages: [] })));

    expect(modClick(linkInThread())).toBe(false);
    await waitFor(() =>
      expect(openedExternally(systemOpen, 'https://example.com/docs')).toBe(true),
    );
    expect(act).not.toHaveBeenCalled();
  });

  it('falls back to the plain behaviour for a link outside any thread', async () => {
    const systemOpen = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ threadId: '', kind: 'state', pages: [] })));
    const link = document.createElement('a');
    link.href = 'https://example.com/docs';
    document.body.appendChild(link);

    expect(modClick(link)).toBe(false);
    await waitFor(() =>
      expect(openedExternally(systemOpen, 'https://example.com/docs')).toBe(true),
    );
    expect(act).not.toHaveBeenCalled();
  });

  it('keeps middle-click on the system browser, modifier or not', async () => {
    const systemOpen = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ threadId: '', kind: 'state', pages: [] })));
    const link = linkInThread();

    link.dispatchEvent(
      new MouseEvent('auxclick', { bubbles: true, cancelable: true, button: 1, metaKey: true }),
    );
    await waitFor(() =>
      expect(openedExternally(systemOpen, 'https://example.com/docs')).toBe(true),
    );
    expect(act).not.toHaveBeenCalled();
  });
});
