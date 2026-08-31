// The top fade and the scrolling content paint in separate compositor layers.
// Real geometry is required: happy-dom cannot prove that the overlay crosses
// the scroll clip's device-pixel boundary without shortening the visible fade.
import { describe, expect, it } from 'vitest';
import '../../../app.css';
import {
  mountTimeline,
  seedTimelineItems,
  setupTimelineHarness,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';

setupTimelineHarness();

const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };
const FADE_PX = 32;
const SCROLLBAR_SAFE_PX = 16;

describe('message timeline — top fade compositor seam', () => {
  it('overdraws the clip by one opaque pixel without shortening the fade', async () => {
    const threadId = 'thread-top-fade-edge';
    const items = seedTimelineItems(threadId, {
      question: (i) => `Question ${i}: keep enough history above the viewport.`,
      replyLead: (i) => `Reply ${i}: exercise the real timeline geometry.`,
      replyList: '- first measured line\n- second measured line\n- third measured line',
    });
    const { scrollEl } = await mountTimeline(threadId, items, QUIET_BOTTOM);
    const fade = scrollEl.parentElement?.querySelector('[data-testid="message-timeline-top-fade"]');
    expect(fade).toBeInstanceOf(HTMLElement);

    const scrollRect = scrollEl.getBoundingClientRect();
    const fadeRect = (fade as HTMLElement).getBoundingClientRect();
    expect(fadeRect.top).toBeCloseTo(scrollRect.top - 1, 2);
    expect(fadeRect.bottom).toBeCloseTo(scrollRect.top + FADE_PX, 2);
    expect(scrollRect.right - fadeRect.right).toBeCloseTo(SCROLLBAR_SAFE_PX, 2);
    expect(getComputedStyle(fade as HTMLElement).backgroundImage).toContain('1px');
  });
});
