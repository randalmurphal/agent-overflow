import { afterEach, describe, expect, it } from 'vitest';
import { sharedGlobalChanges, snapshotSharedGlobals } from './sharedGlobals';

afterEach(() => {
  delete (navigator as { clipboard?: unknown }).clipboard;
  delete (Element.prototype as { addEventListener?: unknown }).addEventListener;
});

describe('sharedGlobalChanges', () => {
  // happy-dom's clipboard is a prototype getter over a private field, which
  // answers undefined when read on the prototype itself.
  it('reports an own property left undefined over an inherited accessor', () => {
    const before = snapshotSharedGlobals();
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
    expect(sharedGlobalChanges(before)).toEqual(['navigator.clipboard was added']);
  });

  // What a spy restored on an inherited method can leave behind.
  it('ignores an own copy of an inherited method', () => {
    const before = snapshotSharedGlobals();
    Object.defineProperty(Element.prototype, 'addEventListener', {
      value: EventTarget.prototype.addEventListener, configurable: true, writable: true,
    });
    expect(sharedGlobalChanges(before)).toEqual([]);
  });
});
