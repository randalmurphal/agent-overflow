import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { registerCodeCopyListener } from './codeCopy';

describe('codeCopy delegated click listener', () => {
  const originalWriteText = navigator.clipboard?.writeText;

  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    document.body.innerHTML = '';
    if (originalWriteText && navigator.clipboard) {
      Object.defineProperty(navigator.clipboard, 'writeText', {
        value: originalWriteText,
        configurable: true,
        writable: true,
      });
    }
  });

  it('copies the sibling <pre> textContent when a .ch-copy button is clicked', async () => {
    const writeText = vi.fn<(s: string) => Promise<void>>(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    });

    registerCodeCopyListener();

    const wrap = document.createElement('div');
    wrap.className = 'ch-wrap';
    wrap.innerHTML =
      '<button type="button" class="ch-copy" aria-label="Copy code">Copy</button>' +
      '<pre class="ch-chroma"><code><span class="ch-k">func</span> <span class="ch-nf">main</span>() {}</code></pre>';
    document.body.appendChild(wrap);

    const btn = wrap.querySelector<HTMLButtonElement>('button.ch-copy');
    btn!.click();
    await Promise.resolve();

    expect(writeText).toHaveBeenCalledTimes(1);
    const copied = writeText.mock.calls[0][0];
    expect(copied).toContain('func');
    expect(copied).toContain('main');
    expect(copied).not.toContain('<span');
    expect(btn!.dataset.copyState).toBe('copied');
    expect(btn!.textContent).toBe('Copied');

    vi.advanceTimersByTime(1300);
    await Promise.resolve();
    expect(btn!.dataset.copyState).toBeUndefined();
    expect(btn!.textContent).toBe('Copy');
  });

  it('ignores clicks outside .ch-copy', async () => {
    const writeText = vi.fn<(s: string) => Promise<void>>(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    });

    registerCodeCopyListener();

    const unrelated = document.createElement('button');
    unrelated.textContent = 'Other';
    document.body.appendChild(unrelated);
    unrelated.click();
    await Promise.resolve();
    expect(writeText).not.toHaveBeenCalled();
  });
});
