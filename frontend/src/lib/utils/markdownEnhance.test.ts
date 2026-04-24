import { beforeEach, describe, expect, it, vi } from 'vitest';
import { enhanceMarkdown } from './markdownEnhance';
import mermaid from 'mermaid';

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
  });

  it('skips copy-button DOM work while the markdown row is streaming', async () => {
    const container = codeContainer();

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: true,
      isCurrent: () => true,
    });

    expect(container.querySelector('[data-code-copy]')).toBeNull();
  });

  it('attaches copy buttons after a row settles', async () => {
    const container = codeContainer();

    await enhanceMarkdown(container, {
      generation: 1,
      renderScope: 'test',
      streaming: false,
      isCurrent: () => true,
    });

    expect(container.querySelector('[data-code-copy]')?.textContent).toBe('Copy');
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

  it('does not attach throwaway copy buttons before preparing Mermaid placeholders', async () => {
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

    expect(container.querySelector('[data-code-copy]')).toBeNull();
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
    expect(pre?.querySelector('[data-code-copy]')).not.toBeNull();
  });
});
