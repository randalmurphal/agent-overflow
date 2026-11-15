import { vi } from 'vitest';
import { reviewFindTurn } from '../../lib/components/review/ReviewPaneFind.svelte';

// Holds the review pane's find scans at their pauses. While held, the
// scan's clock (reviewFindTurn.now) reads every slice as spent, so a scan
// pauses at its first clock read, after CLOCK_ROWS (reviewFindScan.ts)
// units of work, and waits there; content with fewer units finishes without pausing.
// Released, scans run on the real clock. Restore it in the test's finally.

export interface ReviewFindHold {
  /** Holds every scan from its next clock read. */
  hold(): void;
  /** Scans waiting at a pause now. */
  readonly held: number;
  /** Lets every waiting scan go on, on the real clock. */
  release(): void;
  restore(): void;
}

export function holdReviewFindScans(): ReviewFindHold {
  const realNow = reviewFindTurn.now;
  const realPause = reviewFindTurn.pause;
  const waiting: (() => void)[] = [];
  let holding = false;
  const now = vi.spyOn(reviewFindTurn, 'now').mockImplementation(() => (holding ? Infinity : realNow()));
  const pause = vi.spyOn(reviewFindTurn, 'pause').mockImplementation(() => (holding ? new Promise<void>((resolve) => waiting.push(resolve)) : realPause()));
  const release = () => {
    holding = false;
    for (const resolve of waiting.splice(0)) resolve();
  };
  return {
    hold() {
      holding = true;
    },
    get held() {
      return waiting.length;
    },
    release,
    restore() {
      release();
      now.mockRestore();
      pause.mockRestore();
    },
  };
}
