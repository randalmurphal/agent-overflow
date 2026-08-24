import { describe, expect, it } from 'vitest';
import '../../../app.css';
import { fireEvent, render } from '@testing-library/svelte';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import SubagentBodyClipHarness from './SubagentBodyClipHarness.svelte';

function distanceFromBottom(element: HTMLElement): number {
  return element.scrollHeight - element.clientHeight - element.scrollTop;
}

function firstVisibleRow(clip: HTMLElement): { text: string; top: number } {
  const clipTop = clip.getBoundingClientRect().top;
  for (const row of clip.querySelectorAll<HTMLElement>('[data-testid="clip-row"]')) {
    const rect = row.getBoundingClientRect();
    if (rect.bottom > clipTop + 1) return { text: row.textContent ?? '', top: rect.top };
  }
  throw new Error('no visible clip row');
}

describe('subagent body clip', () => {
  it('caps and virtualizes a long digest, then follows appended work at the bottom', async () => {
    const { getByTestId, container } = render(SubagentBodyClipHarness);
    const clip = getByTestId('subagent-group-scroll');

    await waitFor(
      () => clip.scrollHeight > clip.clientHeight && clip.clientHeight > 0,
      'subagent digest to become scrollable',
      240,
    );
    await waitFor(
      () => Math.abs(distanceFromBottom(clip)) <= 2,
      'subagent digest initial bottom',
      360,
    );

    expect(clip.clientHeight).toBeLessThanOrEqual(Math.min(window.innerHeight / 2, 320) + 1);
    expect(container.querySelectorAll('[data-testid="clip-row"]').length).toBeLessThan(180);

    await fireEvent.click(getByTestId('append-row'));
    for (let i = 0; i < 4; i += 1) await raf();
    await waitFor(
      () => Math.abs(distanceFromBottom(clip)) <= 2,
      'subagent digest appended bottom',
      360,
    );
    expect(container.textContent).toContain('Read row 180');
  });

  it('hands wheel control to the reader and resumes follow only after returning to bottom', async () => {
    const { getByTestId } = render(SubagentBodyClipHarness);
    const clip = getByTestId('subagent-group-scroll');
    await waitFor(
      () =>
        clip.scrollHeight > clip.clientHeight &&
        Math.abs(distanceFromBottom(clip)) <= 2 &&
        getByTestId('overlay-scrollbar').dataset.visible === 'true',
      'scrollable bottom-following digest',
      360,
    );
    for (let i = 0; i < 4; i += 1) await raf();

    const bar = getByTestId('overlay-scrollbar');
    await fireEvent.pointerDown(bar, {
      button: 0,
      clientY: bar.getBoundingClientRect().top + 2,
      pointerId: 1,
    });
    await raf();
    await waitFor(() => distanceFromBottom(clip) > 200, 'reader scroll escape', 120);
    await raf();
    await raf();
    const readingRow = firstVisibleRow(clip);

    await fireEvent.click(getByTestId('append-row'));
    for (let i = 0; i < 8; i += 1) await raf();
    const retainedRow = firstVisibleRow(clip);
    expect(retainedRow.text).toBe(readingRow.text);
    expect(distanceFromBottom(clip)).toBeGreaterThan(200);

    await fireEvent.wheel(bar, { deltaY: 9_000, deltaMode: 0 });
    await waitFor(() => Math.abs(distanceFromBottom(clip)) <= 2, 'bottom re-arm', 120);
    await fireEvent.click(getByTestId('append-row'));
    await waitFor(
      () => Math.abs(distanceFromBottom(clip)) <= 2,
      'bottom follow after reader returns',
      360,
    );
  });
});
