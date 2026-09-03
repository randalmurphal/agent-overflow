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
import { setPageGrantsFromBootstrap } from '../transport/scopes';

describe('ensurePathLinkClickDelegate', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    __resetPathLinkDelegateForTest();
    setPageGrantsFromBootstrap(false);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    __resetPathLinkDelegateForTest();
    setPageGrantsFromBootstrap(false);
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

  it('suppresses middle-click auxclick without invoking the editor', () => {
    // A middle-click "open in new tab" on the unregistered custom
    // scheme would become an external-protocol-handler request; the
    // auxclick delegate must eat it, and it must NOT open the editor.
    const mock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    ensurePathLinkClickDelegate();
    const link = mountAnchor(buildPathLinkHref('src/foo.ts', undefined, undefined, ''));

    const event = new MouseEvent('auxclick', { bubbles: true, cancelable: true, button: 1 });
    const allowed = link.dispatchEvent(event);

    expect(allowed).toBe(false);
    expect(mock).not.toHaveBeenCalled();
  });

  it('leaves auxclick alone on ordinary anchors', () => {
    setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    ensurePathLinkClickDelegate();
    const link = mountAnchor('https://example.com/foo.ts');

    const event = new MouseEvent('auxclick', { bubbles: true, cancelable: true, button: 1 });
    expect(link.dispatchEvent(event)).toBe(true);
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

  it('suppresses an already-rendered path link after the session becomes view-only', () => {
    const mock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    ensurePathLinkClickDelegate();
    const link = mountAnchor(buildPathLinkHref('src/foo.ts', undefined, undefined, '/repo'));
    setPageGrantsFromBootstrap(true);

    const event = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
    expect(link.dispatchEvent(event)).toBe(false);
    expect(mock).not.toHaveBeenCalled();
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

  // The selection path writes through the DataTransfer the copy event
  // already owns, not `navigator.clipboard.write` — inside a copy event
  // `setData` is the synchronous, permission-free, every-engine API, and
  // an async write there would race the event's own clipboard fill. The
  // flavors themselves come from the same helper the Copy buttons use.
  describe('text/html flavor', () => {
    function selectAndCopy(html: string): ClipboardEvent {
      ensureMarkdownCopyDelegate();
      const host = document.createElement('div');
      host.className = 'markdown-body';
      host.innerHTML = html;
      document.body.appendChild(host);

      const range = document.createRange();
      range.selectNodeContents(host);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);

      return dispatchCopy(host);
    }

    it('carries an html flavor alongside the markdown one', () => {
      const event = selectAndCopy('<h2>Title</h2><p>see <strong>bold</strong></p>');

      expect(event.clipboardData?.getData('text/plain')).toBe(
        '## Title\n\nsee **bold**',
      );
      expect(event.clipboardData?.getData('text/html')).toBe(
        '<h2>Title</h2><p>see <strong>bold</strong></p>',
      );
    });

    it('keeps table structure in the html flavor', () => {
      const event = selectAndCopy(
        '<table><thead><tr><th>A</th></tr></thead><tbody><tr><td>1</td></tr></tbody></table>',
      );

      expect(event.clipboardData?.getData('text/plain')).toBe('| A |\n| --- |\n| 1 |');
      expect(event.clipboardData?.getData('text/html')).toBe(
        '<table><thead><tr><th>A</th></tr></thead><tbody><tr><td>1</td></tr></tbody></table>',
      );
    });

    it('copies the source of a rendered Mermaid host in both clipboard flavors', () => {
      const event = selectAndCopy(
        '<p>before</p>'
        + '<div class="mermaid streamdown-mermaid-host mermaid-rendered" '
        + 'data-mermaid-source="graph TD&#10;A to B">'
        + '<pre class="mermaid-source-fallback" aria-hidden="true">graph TD\nA to B</pre>'
        + '<div data-streamdown-mermaid="diagram-1"><div>'
        + '<svg data-mermaid-svg><svg><text>Rendered label</text></svg></svg>'
        + '</div></div></div>'
        + '<p>after</p>',
      );

      expect(event.clipboardData?.getData('text/plain')).toBe(
        'before\n\n```mermaid\ngraph TD\nA to B\n```\n\nafter',
      );
      const html = event.clipboardData?.getData('text/html') ?? '';
      expect(html).toContain('<code class="language-mermaid">graph TD\nA to B</code>');
      expect(html).not.toContain('Rendered label');
    });

    it('leaves the html flavor unset when the selection has no renderable markdown', () => {
      // A lone image renders as its alt text in the html flavor, and an
      // empty alt leaves nothing to write — the plain flavor still
      // carries the markdown.
      const event = selectAndCopy('<p><img src="https://example.test/i.png" alt=""></p>');

      expect(event.clipboardData?.getData('text/plain')).toBe(
        '![](https://example.test/i.png)',
      );
      expect(event.clipboardData?.getData('text/html')).toBe('');
    });
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

  // Multi-range selections (Gecko ctrl+click / ctrl+drag) used to lose
  // everything after the first range. happy-dom follows Blink and keeps a
  // single range through `addRange`, so the selection itself has to be
  // stubbed — there is no way to build a two-range Selection in this
  // environment, and the delegate reads nothing from it but `rangeCount`
  // and `getRangeAt`.
  describe('multi-range selections', () => {
    let selectionSpy: ReturnType<typeof vi.spyOn> | null = null;

    afterEach(() => {
      selectionSpy?.mockRestore();
      selectionSpy = null;
    });

    function stubSelection(ranges: Range[]): void {
      const fake = {
        rangeCount: ranges.length,
        getRangeAt: (index: number) => ranges[index],
      } as unknown as Selection;
      selectionSpy = vi.spyOn(window, 'getSelection').mockReturnValue(fake);
    }

    function rangeOver(el: Element): Range {
      const range = document.createRange();
      range.selectNodeContents(el);
      return range;
    }

    /** Selects the element itself, so a block's own tag reaches the walker
     *  (selecting a list's CONTENTS clones bare <li>s with no owning list). */
    function rangeOverNode(el: Element): Range {
      const range = document.createRange();
      range.selectNode(el);
      return range;
    }

    function markdownHost(html: string): HTMLElement {
      const host = document.createElement('div');
      host.className = 'markdown-body';
      host.innerHTML = html;
      document.body.appendChild(host);
      return host;
    }

    it('serializes every range, in document order, whatever order they were added', () => {
      ensureMarkdownCopyDelegate();
      const host = markdownHost(
        '<p id="first">alpha <strong>one</strong></p>'
        + '<p id="second">beta</p>'
        + '<p id="third">gamma</p>',
      );
      // Added last-to-first: `addRange` appends, so this is the order a
      // user who ctrl+clicked bottom-up produces.
      stubSelection([
        rangeOver(host.querySelector('#third')!),
        rangeOver(host.querySelector('#first')!),
        rangeOver(host.querySelector('#second')!),
      ]);

      const event = dispatchCopy(host);

      expect(event.defaultPrevented).toBe(true);
      expect(event.clipboardData?.getData('text/plain')).toBe(
        'alpha **one**\n\nbeta\n\ngamma',
      );
    });

    it('ignores a collapsed range instead of injecting a separator', () => {
      ensureMarkdownCopyDelegate();
      const host = markdownHost('<p id="first">alpha</p><p id="second">beta</p>');
      const caret = document.createRange();
      caret.setStart(host.querySelector('#second')!.firstChild!, 2);
      caret.collapse(true);
      expect(caret.collapsed).toBe(true);
      stubSelection([
        rangeOver(host.querySelector('#first')!),
        caret,
        rangeOver(host.querySelector('#second')!),
      ]);

      const event = dispatchCopy(host);

      expect(event.clipboardData?.getData('text/plain')).toBe('alpha\n\nbeta');
    });

    it('leaves the clipboard alone when every range is collapsed', () => {
      ensureMarkdownCopyDelegate();
      const host = markdownHost('<p id="first">alpha</p>');
      const caret = document.createRange();
      caret.setStart(host.querySelector('#first')!.firstChild!, 1);
      caret.collapse(true);
      stubSelection([caret]);

      const event = dispatchCopy(host);

      expect(event.defaultPrevented).toBe(false);
      expect(event.clipboardData?.getData('text/plain')).toBe('');
    });

    it('does not let a bare caret inside markdown claim a selection made elsewhere', () => {
      // The caret's own range rides along in some ctrl+drag sequences. It is
      // not something the user selected, so it must not pull a plain-text
      // selection through the markdown serializer.
      ensureMarkdownCopyDelegate();
      const host = markdownHost('<p id="first">alpha</p>');
      const outside = document.createElement('div');
      outside.innerHTML = '<p id="chrome">plain prose</p>';
      document.body.appendChild(outside);
      const caret = document.createRange();
      caret.setStart(host.querySelector('#first')!.firstChild!, 1);
      caret.collapse(true);
      stubSelection([caret, rangeOver(outside.querySelector('#chrome')!)]);

      const event = dispatchCopy(outside);

      expect(event.defaultPrevented).toBe(false);
      expect(event.clipboardData?.getData('text/plain')).toBe('');
    });

    it('claims the copy when any one range is inside markdown, and keeps the others', () => {
      ensureMarkdownCopyDelegate();
      const outside = document.createElement('div');
      outside.innerHTML = '<p id="chrome">12:34 PM</p>';
      document.body.appendChild(outside);
      const host = markdownHost('<ol><li>foo</li></ol>');
      // Chrome first in document order, markdown second.
      stubSelection([
        rangeOver(outside.querySelector('#chrome')!),
        rangeOverNode(host.querySelector('ol')!),
      ]);

      const event = dispatchCopy(host);

      expect(event.defaultPrevented).toBe(true);
      expect(event.clipboardData?.getData('text/plain')).toBe('12:34 PM\n\n1. foo');
    });

    it('stays out of a multi-range selection with no markdown in it', () => {
      ensureMarkdownCopyDelegate();
      const outside = document.createElement('div');
      outside.innerHTML = '<p id="a">plain one</p><p id="b">plain two</p>';
      document.body.appendChild(outside);
      stubSelection([
        rangeOver(outside.querySelector('#a')!),
        rangeOver(outside.querySelector('#b')!),
      ]);

      const event = dispatchCopy(outside);

      expect(event.defaultPrevented).toBe(false);
      expect(event.clipboardData?.getData('text/plain')).toBe('');
    });
  });
});
