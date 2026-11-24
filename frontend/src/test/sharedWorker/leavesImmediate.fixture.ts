// Leaves an element whose removal schedules work on Node's own
// setImmediate, which no file tracks: it runs after this file retired, once
// Vitest waits on its report, and adds a window listener while no file is
// open.
import { setImmediate } from 'node:timers';
import { it } from 'vitest';
import { listen } from './fixtureState';

class Outliver extends HTMLElement {
  disconnectedCallback(): void {
    setImmediate(() => listen('outlived'));
  }
}

it('leaves an element whose removal schedules untracked work', () => {
  if (customElements.get('ao-fixture-outliver') === undefined) customElements.define('ao-fixture-outliver', Outliver);
  document.body.append(document.createElement('ao-fixture-outliver'));
});
