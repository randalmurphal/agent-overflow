import { afterEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import {
  applySettingsSnapshot,
  getSettings,
  resetSettingsForTest,
} from '../../stores/settings.svelte';
import { waitFor } from '../../../test/helpers/browserFrames';
import {
  mountTimeline,
  seedTimelineItems,
  setupTimelineHarness,
} from '../../../test/helpers/timelineBrowserHarness';

const QUIET_BOTTOM = {
  epsilonPx: 2,
  stableFrames: 12,
  frameBudget: 360,
};

const PROSE = {
  question: (index: number) => `Question ${index} stays short.`,
  replyLead: (index: number) => `Reply ${index} stays short too.`,
  replyList: '- short',
};

setupTimelineHarness();

afterEach(() => {
  document.documentElement.style.removeProperty('font-size');
  resetSettingsForTest();
});

describe('timeline user-message overflow measurement', () => {
  it('remeasures a mounted message when the root font size changes', async () => {
    resetSettingsForTest();
    const threadId = 'user-overflow-typography';
    const items = seedTimelineItems(threadId, PROSE);
    const target = items[items.length - 2];
    target.summary = Array.from(
      { length: 18 },
      (_, index) => `sentence ${index} about a typography-only reflow`,
    ).join(' ');
    const { host } = await mountTimeline(threadId, items, QUIET_BOTTOM);
    const row = host.querySelector(`[data-item-id="${target.id}"]`);
    if (!row) throw new Error('target user row did not mount');
    const toggle = () => row.querySelector('[data-testid="user-message-clamp-toggle"]');

    expect(toggle()).toBeNull();

    document.documentElement.style.fontSize = `${(20 * 16) / 13}px`;
    applySettingsSnapshot({ ...getSettings(), fontSize: 20 });
    await waitFor(() => toggle() !== null, 'overflow toggle after font-scale reflow');

    document.documentElement.style.removeProperty('font-size');
    applySettingsSnapshot({ ...getSettings(), fontSize: 13 });
    await waitFor(() => toggle() === null, 'overflow toggle retires after font-scale restore');
  });
});
