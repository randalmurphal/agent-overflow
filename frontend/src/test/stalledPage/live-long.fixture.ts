// A live browser file that reports a result every second for longer than
// the check's bound, for scripts/check-stalled-page-watchdog.mjs.
import { test } from 'vitest';

for (let i = 0; i < 14; i += 1) {
  test(`live step ${i}`, () => new Promise<void>((resolve) => setTimeout(resolve, 1_000)));
}
