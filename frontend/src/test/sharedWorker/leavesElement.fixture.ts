// Leaves an element in the body whose removal adds a window listener and
// starts an import. The document reset removes it while the file retires,
// so both are this file's.
import { it } from 'vitest';
import { listen } from './fixtureState';

class Leaver extends HTMLElement {
  disconnectedCallback(): void {
    listen('teardown');
    void import('./teardownModule');
  }
}

it('leaves an element that adds a listener and starts an import when it is removed', () => {
  if (customElements.get('ao-fixture-leaver') === undefined) customElements.define('ao-fixture-leaver', Leaver);
  document.body.append(document.createElement('ao-fixture-leaver'));
});
