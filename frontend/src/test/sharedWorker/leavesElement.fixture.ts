// Leaves an element in the body whose removal adds a window listener. The
// document reset removes it after the file's listeners were released, so
// that listener is installed while no file is open.
import { it } from 'vitest';
import { listen } from './fixtureState';

class Leaver extends HTMLElement {
  disconnectedCallback(): void {
    listen('leaver');
  }
}

it('leaves an element that adds a listener when it is removed', () => {
  if (customElements.get('ao-fixture-leaver') === undefined) customElements.define('ao-fixture-leaver', Leaver);
  document.body.append(document.createElement('ao-fixture-leaver'));
});
