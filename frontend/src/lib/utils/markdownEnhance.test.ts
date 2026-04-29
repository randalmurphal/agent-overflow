import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { waitFor } from '@testing-library/svelte';
import {
  enhanceMarkdown,
  __resetPathLinkDelegateForTest,
  __resetMermaidSvgCacheForTest,
} from './markdownEnhance';
import { __resetMarkdownCopyDelegateForTest } from './markdownCopyDelegate';
import mermaid from 'mermaid';
import { setBindingMock, getBindingMock } from '../../test/mocks/bindings-app';

vi.mock('mermaid', () => ({
  default: {
    initialize: vi.fn(),
    render: vi.fn(async () => ({
      svg: '<svg xmlns="http://www.w3.org/2000/svg"><text>Idea</text><foreignObject><div>HTML label</div></foreignObject></svg>',
    })),
  },
}));

function codeContainer(): HTMLElement {
  const container = document.createElement('div');
  container.innerHTML = '<pre><code>console.log("ok")</code></pre>';
  return container;
}

describe('enhanceMarkdown', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    __resetMermaidSvgCacheForTest();
  });

  it('skips copy-button DOM work while the markdown row is streaming', async () => {
    const container = codeContainer();

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: true,
      isCurrent: () => true,
    });

    expect(container.querySelector('[data-code-copy-mount]')).toBeNull();
  });

  it('attaches copy buttons after a row settles', async () => {
    const container = codeContainer();

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    const wrapper = container.querySelector<HTMLElement>('[data-code-copy-mount]');
    expect(wrapper).not.toBeNull();
    const button = wrapper?.querySelector('button');
    expect(button?.getAttribute('aria-label')).toBe('Copy code');
    expect(button?.querySelector('svg')).not.toBeNull();
  });

  it('writes the raw code to the clipboard when the copy button is clicked', async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    });
    const container = codeContainer();

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    const button = container.querySelector<HTMLButtonElement>('[data-code-copy-mount] button');
    button?.click();
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('console.log("ok")'));
  });

  it('renders Mermaid with SVG text labels that survive sanitization', async () => {
    const container = document.createElement('div');
    container.innerHTML = `
      <pre><code class="language-mermaid">flowchart TD
        A[Idea] --> B[Build it]
      </code></pre>
    `;

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    expect(mermaid.initialize).toHaveBeenCalledWith(expect.objectContaining({
      securityLevel: 'strict',
      htmlLabels: false,
    }));
    const pre = container.querySelector('pre');
    expect(pre?.classList.contains('mermaid-rendered')).toBe(true);
    expect(pre?.textContent).toContain('Idea');
    expect(pre?.innerHTML).not.toContain('foreignObject');
  });

  it('reserves Mermaid position before the async renderer resolves', async () => {
    let resolveRender: (value: { svg: string; diagramType: string }) => void = () => {};
    vi.mocked(mermaid.render).mockReturnValueOnce(new Promise((resolve) => {
      resolveRender = resolve;
    }));
    const container = document.createElement('div');
    container.innerHTML = `
      <p>Before</p>
      <pre><code class="language-mermaid">flowchart TD
        A[Before] --> B[After]
      </code></pre>
      <p>After</p>
    `;

    const pending = enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    const pre = container.querySelector('pre');
    expect(pre?.classList.contains('mermaid-pending')).toBe(true);
    expect(pre?.textContent).toContain('Rendering diagram...');
    expect(Array.from(container.children).map((child) => child.tagName)).toEqual(['P', 'PRE', 'P']);

    resolveRender({
      svg: '<svg xmlns="http://www.w3.org/2000/svg"><text>Before</text><text>After</text></svg>',
      diagramType: 'flowchart-v2',
    });
    await pending;

    expect(pre?.classList.contains('mermaid-pending')).toBe(false);
    expect(pre?.classList.contains('mermaid-rendered')).toBe(true);
    expect(pre?.textContent).toContain('Before');
    expect(pre?.textContent).toContain('After');
    expect(Array.from(container.children).map((child) => child.tagName)).toEqual(['P', 'PRE', 'P']);
  });

  it('attaches a single copy-source button to a rendered Mermaid block', async () => {
    const container = document.createElement('div');
    container.innerHTML = `
      <pre><code class="language-mermaid">flowchart TD
        A[Before] --> B[After]
      </code></pre>
    `;

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    const wrappers = container.querySelectorAll('[data-code-copy-mount]');
    expect(wrappers).toHaveLength(1);
    expect(wrappers[0]?.querySelector('button')?.getAttribute('aria-label')).toBe(
      'Copy diagram source',
    );
  });

  it('writes the original Mermaid source to the clipboard on click', async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    });
    const source = 'flowchart LR\nX-->Y';
    const container = document.createElement('div');
    const code = document.createElement('code');
    code.className = 'language-mermaid';
    code.textContent = source;
    const pre = document.createElement('pre');
    pre.appendChild(code);
    container.appendChild(pre);

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    const button = container.querySelector<HTMLButtonElement>('[data-code-copy-mount] button');
    button?.click();
    // The button copies the original code text — proves it's not pulling
    // from the rendered SVG textContent.
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(source));
  });

  it('reuses the cached SVG when the same Mermaid source is rendered twice', async () => {
    // Why: virtua's overscan eviction unmounts assistant-message rows;
    // re-mounting would otherwise re-invoke `mermaid.render` (50–500ms)
    // and the row would mount-at-estimate-then-pop. The module-level
    // SVG cache deduplicates by source-text hash so a remount paints
    // synchronously from cache.
    const source = 'flowchart TD\nA[Idea] --> B[Build it]';
    function buildContainer(): HTMLElement {
      const container = document.createElement('div');
      container.innerHTML = `<pre><code class="language-mermaid">${source}</code></pre>`;
      return container;
    }

    const a = buildContainer();
    const b = buildContainer();

    await enhanceMarkdown(a, { generation: 1, renderScope: 'test', streaming: false, isCurrent: () => true });
    const callsAfterFirst = vi.mocked(mermaid.render).mock.calls.length;
    await enhanceMarkdown(b, { generation: 1, renderScope: 'test', streaming: false, isCurrent: () => true });
    const callsAfterSecond = vi.mocked(mermaid.render).mock.calls.length;

    expect(callsAfterSecond).toBe(callsAfterFirst);
    expect(b.querySelector('pre')?.classList.contains('mermaid-rendered')).toBe(true);
  });

  it('restores the Mermaid source when initialization fails', async () => {
    vi.mocked(mermaid.initialize).mockImplementationOnce(() => {
      throw new Error('init failed');
    });
    const container = document.createElement('div');
    container.innerHTML = `
      <pre><code class="language-mermaid">flowchart TD
        A[Before] --> B[After]
      </code></pre>
    `;

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    const pre = container.querySelector('pre');
    expect(pre?.classList.contains('mermaid-pending')).toBe(false);
    expect(pre?.classList.contains('mermaid-error')).toBe(true);
    expect(pre?.querySelector('code.language-mermaid')?.textContent).toContain('flowchart TD');
    expect(pre?.querySelector('[data-code-copy-mount]')).not.toBeNull();
  });
});

describe('enhanceMarkdown — path linkify', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    __resetPathLinkDelegateForTest();
  });

  it('replaces inline file paths in prose with editor-link anchors', async () => {
    const container = document.createElement('div');
    container.innerHTML = '<p>see src/lib/foo.ts:12 for context</p>';

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    const link = container.querySelector('a.editor-link');
    expect(link).not.toBeNull();
    expect(link?.getAttribute('data-path')).toBe('src/lib/foo.ts');
    expect(link?.getAttribute('data-line')).toBe('12');
    expect(link?.textContent).toBe('src/lib/foo.ts:12');
  });

  it('does not linkify paths inside <pre><code> blocks', async () => {
    const container = document.createElement('div');
    container.innerHTML = '<pre><code>see src/lib/foo.ts here</code></pre>';

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    expect(container.querySelector('a.editor-link')).toBeNull();
  });

  it('linkifies paths inside inline <code> outside <pre>', async () => {
    const container = document.createElement('div');
    container.innerHTML = '<p>see <code>src/lib/foo.ts</code> note</p>';

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    const link = container.querySelector('code a.editor-link');
    expect(link).not.toBeNull();
    expect(link?.getAttribute('data-path')).toBe('src/lib/foo.ts');
  });

  it('skips URL-shaped tokens', async () => {
    const container = document.createElement('div');
    container.innerHTML = '<p>visit https://example.com/foo for docs</p>';

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    expect(container.querySelector('a.editor-link')).toBeNull();
  });

  it('invokes OpenInEditor when an editor-link is clicked', async () => {
    const mock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const container = document.createElement('div');
    container.innerHTML = '<p>see src/lib/foo.ts:12 for context</p>';
    document.body.appendChild(container);

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    const link = container.querySelector('a.editor-link') as HTMLAnchorElement;
    link.click();
    // The delegate dispatches the binding through a dynamic import, so
    // assertion must wait for both the import resolution and the await
    // chain inside the handler before the mock is observed.
    await waitFor(() => {
      expect(mock).toHaveBeenCalledTimes(1);
    });
    expect(mock.mock.calls[0]).toEqual(['src/lib/foo.ts', 12, 0]);
    container.remove();
    void getBindingMock; // keep helper reachable for future cases
  });
});

describe('enhanceMarkdown — copy delegate', () => {
  beforeEach(() => {
    __resetMarkdownCopyDelegateForTest();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    __resetMarkdownCopyDelegateForTest();
  });

  async function installDelegate(): Promise<void> {
    // The delegate installs lazily inside enhanceMarkdown, ahead of
    // the streaming early-return — pass an empty container so the
    // rest of the pipeline is a no-op.
    const probe = document.createElement('div');
    await enhanceMarkdown(probe, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });
  }

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

  it('replaces clipboard text with markdown when the selection is inside .markdown-body', async () => {
    await installDelegate();
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

  it('leaves the clipboard alone when the selection is outside .markdown-body', async () => {
    await installDelegate();
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

  it('does not interfere with a collapsed selection', async () => {
    await installDelegate();
    const host = document.createElement('div');
    host.className = 'markdown-body';
    host.innerHTML = '<p>foo</p>';
    document.body.appendChild(host);

    window.getSelection()?.removeAllRanges();

    const event = dispatchCopy(host);

    expect(event.defaultPrevented).toBe(false);
    expect(event.clipboardData?.getData('text/plain')).toBe('');
  });

  it('rewrites bold/italic/inline-code as markdown markers', async () => {
    await installDelegate();
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

  it('still serializes as markdown when the selection overshoots into adjacent chrome', async () => {
    // Real-world repro: the user drag-selects an assistant message
    // body and lets the cursor settle one char past the end, into
    // the timestamp row below. The commonAncestorContainer in that
    // case is an outer wrapper that has no `.markdown-body`
    // ancestor — but the start endpoint is still inside markdown,
    // so the user's intent is clearly "copy this message as
    // markdown".
    await installDelegate();
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

  it('installs the delegate even when the row is still streaming', async () => {
    // The delegate should arm before the streaming early-return — a
    // user can copy from a settled surface elsewhere in the app while
    // some other row is still streaming.
    const probe = document.createElement('div');
    await enhanceMarkdown(probe, {
      generation: 1,
      renderScope: 'test',
      streaming: true,
      isCurrent: () => true,
    });

    const host = document.createElement('div');
    host.className = 'markdown-body';
    host.innerHTML = '<p>hi</p>';
    document.body.appendChild(host);

    const range = document.createRange();
    range.selectNodeContents(host);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);

    const event = dispatchCopy(host);
    expect(event.defaultPrevented).toBe(true);
    expect(event.clipboardData?.getData('text/plain')).toBe('hi');
  });

  it('bails when the clipboard event has no clipboardData', async () => {
    // Synthetic copy events from extensions / automation can present
    // a null `clipboardData`. The handler should bail without
    // calling preventDefault, leaving the browser default in place.
    await installDelegate();
    const host = document.createElement('div');
    host.className = 'markdown-body';
    host.innerHTML = '<p>hi</p>';
    document.body.appendChild(host);

    const range = document.createRange();
    range.selectNodeContents(host);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);

    const event = new Event('copy', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(event, 'clipboardData', { value: null });
    host.dispatchEvent(event);

    expect(event.defaultPrevented).toBe(false);
  });
});
