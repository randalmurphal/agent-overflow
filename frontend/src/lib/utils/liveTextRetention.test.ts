import { describe, expect, it } from 'vitest';
import { runNodeWorkload } from '../../test/helpers/nodeWorkload';
import { LIVE_WINDOW_KEEP_CHARS, LIVE_WINDOW_RECUT_CHARS } from './liveText';
import { liveTextRetentionWorkload } from './liveTextRetention.workload';

// A reveal delta of 13 or more characters (a long word, or a late frame's
// worth of text) is a slice of the smoother's received text, which the
// smoother joins into one string every few hundred wire deltas and whenever a
// reader asks for the whole text. A kept slice keeps that whole joined string
// alive, so an accumulation that kept the deltas themselves would hold a copy
// of the text per join; shorter deltas are copies, but kept one by one they
// cost several times their length. This measures what a live window and an
// appended text retain after a real smoother streams into them and is dropped.
//
// The workload runs in a bare `node --expose-gc` subprocess because the
// measurement is a whole-heap delta.

function retained(kind: 'window' | 'appended'): { retained: number; chars: number } {
  const output = runNodeWorkload(liveTextRetentionWorkload, { AO_LIVE_TEXT: kind });
  return JSON.parse(output) as { retained: number; chars: number };
}

// Bytes each result retains, for one-byte text (a character is a byte). The
// window may still share the string it was cut from until its next append.
describe('live text retention', () => {
  it('a live window holds only its own characters', () => {
    const { retained: bytes, chars } = retained('window');
    expect(chars).toBeGreaterThanOrEqual(LIVE_WINDOW_KEEP_CHARS);
    expect(bytes).toBeLessThan(2 * LIVE_WINDOW_RECUT_CHARS + 32 * 1024);
  }, 180_000);

  it('appended live text holds only its own characters', () => {
    const { retained: bytes, chars } = retained('appended');
    expect(chars).toBeGreaterThanOrEqual(100_000);
    expect(bytes).toBeLessThan(2 * chars + 64 * 1024);
  }, 180_000);
});
