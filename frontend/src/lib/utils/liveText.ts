// Text accumulated from streaming reveals. A reveal delta is often a slice
// of the smoother's received text, which keeps that whole string alive while
// the slice lives; an accumulation holds copies instead.
//
// LiveTextWindow is the end of a streaming reasoning-tail row's revealed
// text. The collapsed row shows the last lines of that text through
// TailClampedText, which lays out only a window of it. The window cuts only
// just after a '\n', so it stays bounded while the text keeps containing
// newlines; a stretch without one stays in the window whole, and text with
// none at all is the whole text. Appends go through appendCopied, so the
// window holds copies rather than slices of the smoother's received text.
// The smoother keeps the whole text, and the registry retains it again once
// the row settles (stores/threadRevealSmoothers.ts).

import { TAIL_WINDOW_CAP_CHARS, newlineCutOffset } from '../components/chat/tailWindow';

/**
 * `text + delta` as a string that holds only its own characters once `text`
 * is non-empty: join copies both, where `+` would keep `delta`, and whatever
 * it was sliced from, alive for as long as the result lives.
 */
export function appendCopied(text: string, delta: string): string {
  return text === '' ? delta : [text, delta].join('');
}

/** A suffix of a text and where it starts. */
export interface TextWindow {
  /** The text from `start` to its end. */
  readonly text: string;
  /** Offset of `text` in the whole text: 0, or just after a '\n'. */
  readonly start: number;
}

/**
 * Characters a cut keeps. Twice TailClampedText's layout window, so the
 * window starts above the text the clamp renders and moving it leaves the
 * rendered text unchanged.
 */
export const LIVE_WINDOW_KEEP_CHARS = 2 * TAIL_WINDOW_CAP_CHARS;

/** Length at which the window looks for a cut: half the keep length past it. */
export const LIVE_WINDOW_RECUT_CHARS = LIVE_WINDOW_KEEP_CHARS * 3 / 2;

/**
 * An appendable window over a growing text. It starts at 0 or just after a
 * '\n', where wrapping restarts, so a clamp that renders it from its start
 * breaks lines exactly as it would in the whole text.
 */
export class LiveTextWindow {
  private text: string;
  private start = 0;
  // `text` has no '\n' a cut could use before this offset: the last scan
  // found none there.
  private scannedTo = 0;

  constructor(seed: string) {
    this.text = seed;
  }

  append(delta: string): TextWindow {
    const previousLength = this.text.length;
    this.text = appendCopied(this.text, delta);
    if (this.text.length > LIVE_WINDOW_RECUT_CHARS) this.recut(previousLength);
    return { text: this.text, start: this.start };
  }

  // Cut just after the last '\n' that keeps LIVE_WINDOW_KEEP_CHARS. The cut
  // stays before the previous end, so a reader of the previous window can
  // still see where it ended (TailClampedText's append check).
  private recut(previousLength: number): void {
    const keep = Math.max(LIVE_WINDOW_KEEP_CHARS, this.text.length - previousLength + 1);
    const limit = this.text.length - keep;
    const cut = newlineCutOffset(this.text, this.scannedTo, keep);
    if (cut === null) {
      this.scannedTo = Math.max(this.scannedTo, limit);
      return;
    }
    this.text = this.text.slice(cut);
    this.start += cut;
    this.scannedTo = limit - cut;
  }
}
