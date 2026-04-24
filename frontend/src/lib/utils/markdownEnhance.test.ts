import { describe, expect, it } from 'vitest';
import { enhanceMarkdown } from './markdownEnhance';

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
});
