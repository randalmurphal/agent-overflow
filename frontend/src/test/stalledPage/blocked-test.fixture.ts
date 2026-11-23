// A test whose page blocks, for scripts/check-stalled-page-watchdog.mjs. Not a test
// of the suite: only src/test/stalledPage/vitest.config.ts includes it.
import { test } from 'vitest';

test('blocks the page main thread without spinning', () => {
  // Each synchronous request waits on the server's held response, so the
  // main thread runs no JavaScript and burns no CPU. Bounded to 60 s.
  for (let i = 0; i < 120; i += 1) {
    const xhr = new XMLHttpRequest();
    xhr.open('GET', '/__stall/hold', false);
    xhr.send();
  }
});
