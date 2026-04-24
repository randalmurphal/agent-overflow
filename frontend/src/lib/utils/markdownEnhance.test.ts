import { describe, expect, it, vi } from 'vitest';
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
});
