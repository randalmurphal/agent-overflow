// A browser file whose page blocks after its last result has been reported,
// for scripts/check-stalled-page-watchdog.mjs. The test leaves Shift held;
// Vitest's cleanup after the file releases it, and the keyup handler blocks.
import { test } from 'vitest';
import { userEvent } from 'vitest/browser';

test('passes with Shift still held', async () => {
  document.addEventListener('keyup', function holdOnKeyup() {
    // Non-spinning: each synchronous request waits on the server's held
    // response. Bounded to 60 s.
    for (let i = 0; i < 120; i += 1) {
      const xhr = new XMLHttpRequest();
      xhr.open('GET', '/__stall/hold', false);
      xhr.send();
    }
  });
  await userEvent.keyboard('{Shift>}');
});
