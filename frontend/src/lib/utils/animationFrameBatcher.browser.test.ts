import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { describe, expect, it } from 'vitest';
import AnimationFrameBatcherHarness from './AnimationFrameBatcherHarness.svelte';

type Harness = {
  expandAndMeasure(): Promise<number>;
};

describe('animation frame coordination in Chromium', () => {
  it('runs geometry first and still paints the Svelte update in that frame', async () => {
    const view = render(AnimationFrameBatcherHarness);
    const harness = view.component as unknown as Harness;
    const box = view.getByTestId('animation-frame-batcher-box');

    expect(box.getBoundingClientRect().height).toBe(20);
    await expect(harness.expandAndMeasure()).resolves.toBe(20);
    await tick();
    expect(box.getBoundingClientRect().height).toBe(200);
  });
});
