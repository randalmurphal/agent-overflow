import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { waitFor } from '@testing-library/svelte';
import {
  canUseHostOpenExternalURL,
  externalURLForEventTarget,
  handleExternalURL,
  installExternalLinkDelegate,
  safeExternalURL,
} from './externalLinks';
import { buildPathLinkHref } from './pathLinkExtension';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { resetRunMode, setRunMode } from '../../test/runMode';

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
