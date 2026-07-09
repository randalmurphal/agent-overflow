import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { waitFor } from '@testing-library/svelte';
import {
  ensureMarkdownCopyDelegate,
  ensurePathLinkClickDelegate,
  __resetMarkdownCopyDelegateForTest,
  __resetPathLinkDelegateForTest,
} from './markdownEnhance';
import { buildPathLinkHref } from './pathLinkExtension';
import { setBindingMock, getBindingMock } from '../../test/mocks/bindings-app';

describe('ensurePathLinkClickDelegate', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    __resetPathLinkDelegateForTest();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    __resetPathLinkDelegateForTest();
  });

  function mountAnchor(href: string): HTMLAnchorElement {
    const a = document.createElement('a');
    a.href = href;
    a.textContent = href;
    document.body.appendChild(a);
    return a;
  }

  it('forwards clicks on agent-overflow:open anchors to OpenInEditor', async () => {
    const mock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    ensurePathLinkClickDelegate();
    const link = mountAnchor(buildPathLinkHref('src/foo.ts', 42, 7, '/repo'));

    link.click();

    await waitFor(() => {
      expect(mock).toHaveBeenCalledTimes(1);
    });
    expect(mock.mock.calls[0]).toEqual(['src/foo.ts', 42, 7, '/repo', '']);
    void getBindingMock; // helper kept reachable for future cases
  });

  it('forwards line=0/col=0 when the href omits them', async () => {
    const mock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    ensurePathLinkClickDelegate();
    const link = mountAnchor(buildPathLinkHref('a.ts', undefined, undefined, ''));

    link.click();

    await waitFor(() => expect(mock).toHaveBeenCalledTimes(1));
    expect(mock.mock.calls[0]).toEqual(['a.ts', 0, 0, '', '']);
  });

  it('prevents default so the browser does not navigate to the custom scheme', () => {
    setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    ensurePathLinkClickDelegate();
    const link = mountAnchor(buildPathLinkHref('src/foo.ts', undefined, undefined, ''));

    const event = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
    const allowed = link.dispatchEvent(event);

    expect(allowed).toBe(false);
  });

  it('ignores anchors that do NOT use the agent-overflow: scheme', () => {
    const mock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    ensurePathLinkClickDelegate();
    const link = mountAnchor('https://example.com/foo.ts');

    const event = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
    const allowed = link.dispatchEvent(event);

    expect(allowed).toBe(true);
    expect(mock).not.toHaveBeenCalled();
  });

  it('idempotent install — repeated calls do not multi-fire', async () => {
    const mock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    ensurePathLinkClickDelegate();
    ensurePathLinkClickDelegate();
    ensurePathLinkClickDelegate();
    const link = mountAnchor(buildPathLinkHref('src/foo.ts', undefined, undefined, ''));

    link.click();

    await waitFor(() => expect(mock).toHaveBeenCalledTimes(1));
  });
});

describe('markdown-aware copy delegate', () => {
  beforeEach(() => {
    __resetMarkdownCopyDelegateForTest();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    __resetMarkdownCopyDelegateForTest();
  });

  function dispatchCopy(target: Node): ClipboardEvent {
    const clipboardData = new DataTransfer();
    const event = new ClipboardEvent('copy', {
      bubbles: true,
      cancelable: true,
      clipboardData,
    });
    // happy-dom dispatches via the target so the document-level
    // listener sees a bubbling event the same way a real copy would.
    target.dispatchEvent(event);
    return event;
  }

  it('replaces clipboard text with markdown when the selection is inside .markdown-body', () => {
    ensureMarkdownCopyDelegate();
    const host = document.createElement('div');
    host.className = 'markdown-body';
    host.innerHTML = '<ol><li>foo</li><li>bar</li></ol>';
    document.body.appendChild(host);

    const range = document.createRange();
    range.selectNodeContents(host);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);

    const event = dispatchCopy(host);

    expect(event.defaultPrevented).toBe(true);
    expect(event.clipboardData?.getData('text/plain')).toBe('1. foo\n2. bar');
  });

  it('leaves the clipboard alone when the selection is outside .markdown-body', () => {
    ensureMarkdownCopyDelegate();
    const outside = document.createElement('div');
    outside.innerHTML = '<p>plain prose, no markdown surface</p>';
    document.body.appendChild(outside);

    const range = document.createRange();
    range.selectNodeContents(outside);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);

    const event = dispatchCopy(outside);

    expect(event.defaultPrevented).toBe(false);
    expect(event.clipboardData?.getData('text/plain')).toBe('');
  });

  it('does not interfere with a collapsed selection', () => {
    ensureMarkdownCopyDelegate();
    const host = document.createElement('div');
    host.className = 'markdown-body';
    host.innerHTML = '<p>foo</p>';
    document.body.appendChild(host);

    window.getSelection()?.removeAllRanges();

    const event = dispatchCopy(host);

    expect(event.defaultPrevented).toBe(false);
    expect(event.clipboardData?.getData('text/plain')).toBe('');
  });

  it('rewrites bold/italic/inline-code as markdown markers', () => {
    ensureMarkdownCopyDelegate();
    const host = document.createElement('div');
    host.className = 'markdown-body';
    host.innerHTML =
      '<p>see <strong>bold</strong> and <em>italic</em> plus <code>code()</code></p>';
    document.body.appendChild(host);

    const range = document.createRange();
    range.selectNodeContents(host);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);

    const event = dispatchCopy(host);

    expect(event.clipboardData?.getData('text/plain')).toBe(
      'see **bold** and *italic* plus `code()`',
    );
  });

  it('still serializes as markdown when the selection overshoots into adjacent chrome', () => {
    // Real-world repro: the user drag-selects an assistant message
    // body and lets the cursor settle one char past the end, into
    // the timestamp row below. The commonAncestorContainer in that
    // case is an outer wrapper that has no `.markdown-body`
    // ancestor — but the start endpoint is still inside markdown,
    // so the user's intent is clearly "copy this message as
    // markdown".
    ensureMarkdownCopyDelegate();
    const wrapper = document.createElement('div');
    const body = document.createElement('div');
    body.className = 'markdown-body';
    body.innerHTML = '<ol><li>foo</li><li>bar</li></ol>';
    const meta = document.createElement('div');
    meta.textContent = '12:34 PM';
    wrapper.appendChild(body);
    wrapper.appendChild(meta);
    document.body.appendChild(wrapper);

    const startText = body.querySelector('li')!.firstChild as Text;
    const endText = meta.firstChild as Text;
    const range = document.createRange();
    range.setStart(startText, 0);
    range.setEnd(endText, '12:34'.length);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);

    const event = dispatchCopy(wrapper);

    expect(event.defaultPrevented).toBe(true);
    // The list-marker prefix proves the markdown serializer ran;
    // the trailing chrome text comes through as plain text from
    // cloneContents.
    expect(event.clipboardData?.getData('text/plain')).toContain('1. foo');
  });
});
