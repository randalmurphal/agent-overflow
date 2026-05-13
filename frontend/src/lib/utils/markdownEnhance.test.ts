import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { waitFor } from '@testing-library/svelte';
import {
  enhancePathLinks,
  ensureMarkdownCopyDelegate,
  __resetMarkdownCopyDelegateForTest,
  __resetPathLinkDelegateForTest,
} from './markdownEnhance';
import { setBindingMock, getBindingMock } from '../../test/mocks/bindings-app';

describe('enhancePathLinks', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    __resetPathLinkDelegateForTest();
  });

  it('replaces inline file paths in prose with editor-link anchors', () => {
    const container = document.createElement('div');
    container.innerHTML = '<p>see src/lib/foo.ts:12 for context</p>';

    enhancePathLinks(container, '');

    const link = container.querySelector('a.editor-link');
    expect(link).not.toBeNull();
    expect(link?.getAttribute('data-path')).toBe('src/lib/foo.ts');
    expect(link?.getAttribute('data-line')).toBe('12');
    expect(link?.textContent).toBe('src/lib/foo.ts:12');
  });

  it('does not linkify paths inside <pre><code> blocks', () => {
    const container = document.createElement('div');
    container.innerHTML = '<pre><code>see src/lib/foo.ts here</code></pre>';

    enhancePathLinks(container, '');

    expect(container.querySelector('a.editor-link')).toBeNull();
  });

  it('linkifies paths inside inline <code> outside <pre>', () => {
    const container = document.createElement('div');
    container.innerHTML = '<p>see <code>src/lib/foo.ts</code> note</p>';

    enhancePathLinks(container, '');

    const link = container.querySelector('code a.editor-link');
    expect(link).not.toBeNull();
    expect(link?.getAttribute('data-path')).toBe('src/lib/foo.ts');
  });

  it('skips URL-shaped tokens', () => {
    const container = document.createElement('div');
    container.innerHTML = '<p>visit https://example.com/foo for docs</p>';

    enhancePathLinks(container, '');

    expect(container.querySelector('a.editor-link')).toBeNull();
  });

  it('invokes OpenInEditor with the workspacePath stamped at linkify time', async () => {
    const mock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const container = document.createElement('div');
    container.innerHTML = '<p>see src/lib/foo.ts:12 for context</p>';
    document.body.appendChild(container);

    enhancePathLinks(container, '/home/user/repo');

    const link = container.querySelector('a.editor-link') as HTMLAnchorElement;
    link.click();
    await waitFor(() => {
      expect(mock).toHaveBeenCalledTimes(1);
    });
    expect(mock.mock.calls[0]).toEqual(['src/lib/foo.ts', 12, 0, '/home/user/repo']);
    container.remove();
    void getBindingMock; // keep helper reachable for future cases
  });

  it('falls back to empty workspacePath when none was supplied', async () => {
    const mock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const container = document.createElement('div');
    container.innerHTML = '<p>see src/lib/bar.ts for context</p>';
    document.body.appendChild(container);

    enhancePathLinks(container, '');

    const link = container.querySelector('a.editor-link') as HTMLAnchorElement;
    link.click();
    await waitFor(() => {
      expect(mock).toHaveBeenCalledTimes(1);
    });
    expect(mock.mock.calls[0]).toEqual(['src/lib/bar.ts', 0, 0, '']);
    container.remove();
  });

  it('is idempotent — re-running on already-linkified DOM does not double-wrap', () => {
    // ChatMarkdown's $effect re-runs `enhancePathLinks` on every
    // post-streaming source change. The walker explicitly skips text
    // nodes that already live inside an editor-link, so re-runs are
    // no-ops on already-converted spans.
    const container = document.createElement('div');
    container.innerHTML = '<p>see src/lib/foo.ts for context</p>';
    enhancePathLinks(container, '');
    const after1 = container.innerHTML;
    enhancePathLinks(container, '');
    const after2 = container.innerHTML;
    expect(after2).toBe(after1);
    expect(container.querySelectorAll('a.editor-link')).toHaveLength(1);
  });

  describe('allowlist mode (pathRefs from Go validation)', () => {
    it('wraps only paths the allowlist contains', () => {
      // `src/foo.ts` shows up in pathRefs (Go validated). `bogus/x.bad`
      // shape-matches the local regex but is not in the allowlist —
      // it must NOT linkify in allowlist mode.
      const container = document.createElement('div');
      container.innerHTML = '<p>real src/foo.ts and bogus/x.bad nope</p>';
      enhancePathLinks(container, '', [{ path: 'src/foo.ts' }]);
      const links = container.querySelectorAll('a.editor-link');
      expect(links).toHaveLength(1);
      expect(links[0].getAttribute('data-path')).toBe('src/foo.ts');
    });

    it('empty allowlist short-circuits — nothing wraps', () => {
      // The "Go saw nothing real" case: even path-shaped tokens that
      // would have passed the local regex stay plain text.
      const container = document.createElement('div');
      container.innerHTML = '<p>see src/lib/foo.ts and src/lib/bar.ts</p>';
      enhancePathLinks(container, '', []);
      expect(container.querySelectorAll('a.editor-link')).toHaveLength(0);
    });

    it('emits one anchor per occurrence when the same path appears multiple times', () => {
      // The Go side stat's once, the wire carries one PathRef per
      // occurrence — but the frontend doesn't actually consume the
      // per-occurrence metadata; it derives a unique-path Set and
      // wraps every textual occurrence. Either way the user sees all
      // instances linkified.
      const container = document.createElement('div');
      container.innerHTML = '<p>we edited src/foo.ts, but src/foo.ts still has the bug</p>';
      enhancePathLinks(container, '', [{ path: 'src/foo.ts' }]);
      const links = container.querySelectorAll('a.editor-link');
      expect(links).toHaveLength(2);
      for (const link of Array.from(links)) {
        expect(link.getAttribute('data-path')).toBe('src/foo.ts');
        expect(link.textContent).toBe('src/foo.ts');
      }
    });

    it('captures :line:col suffix from text even though allowlist entry has none', () => {
      // The path string `src/foo.ts` is allowlisted; the text adds a
      // line:col suffix. The wrapped span includes the suffix and
      // data-line/data-col are populated from the text — not from
      // the PathRef.
      const container = document.createElement('div');
      container.innerHTML = '<p>see src/foo.ts:42:7 for the bug</p>';
      enhancePathLinks(container, '', [{ path: 'src/foo.ts' }]);
      const link = container.querySelector('a.editor-link');
      expect(link).not.toBeNull();
      expect(link?.getAttribute('data-path')).toBe('src/foo.ts');
      expect(link?.getAttribute('data-line')).toBe('42');
      expect(link?.getAttribute('data-col')).toBe('7');
      expect(link?.textContent).toBe('src/foo.ts:42:7');
    });

    it('@-prefix widens the wrapped span but data-path stays bare', () => {
      // Visual fix: today's regex's lookbehind excluded `@`, so
      // `@src/foo.ts` rendered as `@<link>src/foo.ts</link>` with a
      // floating `@`. The allowlist matcher includes the `@` in the
      // wrapped span when the boundary check on the char before `@`
      // passes — and the click handler still navigates by the bare
      // file path stored on data-path.
      const container = document.createElement('div');
      container.innerHTML = '<p>see @src/foo.ts for context</p>';
      enhancePathLinks(container, '', [{ path: 'src/foo.ts' }]);
      const link = container.querySelector('a.editor-link');
      expect(link).not.toBeNull();
      expect(link?.textContent).toBe('@src/foo.ts');
      expect(link?.getAttribute('data-path')).toBe('src/foo.ts');
    });

    it('rejects email-tail paths even when the path body is allowlisted', () => {
      // `host/path.ts` happens to be in the allowlist (some unrelated
      // workspace file with that name), but the text mentions it
      // inside the email `name@host/path.ts`. The lookbehind on the
      // char before the `@` is `e` (alphanumeric) so the boundary
      // check fails and nothing wraps.
      const container = document.createElement('div');
      container.innerHTML = '<p>contact name@host/path.ts please</p>';
      enhancePathLinks(container, '', [{ path: 'host/path.ts' }]);
      expect(container.querySelectorAll('a.editor-link')).toHaveLength(0);
    });

    it('longest-first sort prevents nested overlap when paths are siblings', () => {
      // Both `src/foo.ts` and `elsewhere/src/foo.ts` are in the
      // allowlist (separate validated files). In a single text where
      // only the longer path appears, the shorter one would naively
      // match inside it — overlap avoidance must prevent a double
      // wrap.
      const container = document.createElement('div');
      container.innerHTML = '<p>see elsewhere/src/foo.ts for the bug</p>';
      enhancePathLinks(container, '', [
        { path: 'src/foo.ts' },
        { path: 'elsewhere/src/foo.ts' },
      ]);
      const links = container.querySelectorAll('a.editor-link');
      expect(links).toHaveLength(1);
      expect(links[0].getAttribute('data-path')).toBe('elsewhere/src/foo.ts');
    });

    it('falls back to local regex when pathRefs is undefined', () => {
      // Non-assistant surfaces (ChannelView, ProposedPlan, etc.) leave
      // pathRefs unset. The local regex still produces today's behavior
      // — including matching `src/lib/foo.ts` without fs validation.
      const container = document.createElement('div');
      container.innerHTML = '<p>see src/lib/foo.ts for context</p>';
      enhancePathLinks(container, '', undefined);
      const link = container.querySelector('a.editor-link');
      expect(link).not.toBeNull();
      expect(link?.getAttribute('data-path')).toBe('src/lib/foo.ts');
    });
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
