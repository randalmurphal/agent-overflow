import { describe, expect, it } from 'vitest';
import { COMPOSITE_KEY_SEPARATOR, compositeKey } from './compositeKey';

// The helper's whole job is that two different tuples never produce one
// key. Injectivity rests on an assumption about the parts, so the tests
// that matter are the ones that break the assumption.
describe('compositeKey', () => {
  it('is injective across the ways a naive join collides', () => {
    const keys = [
      compositeKey('a', 'b'),
      compositeKey('ab'),
      compositeKey('a', '', 'b'),
      compositeKey('a', 'b', ''),
      compositeKey('', 'a', 'b'),
    ];
    expect(new Set(keys).size).toBe(keys.length);
  });

  it('keeps a part that renders identically in another type distinct by position', () => {
    // Positional, so `true` and `'true'` in the SAME slot are equal keys —
    // and that is fine, because a slot only ever carries one kind. What must
    // not collide is the same values in different slots.
    expect(compositeKey('x', true, 1)).not.toBe(compositeKey('x', 1, true));
  });

  it('separator-free ids of every shape the registries use round-trip', () => {
    const ids = ['a"b', 'a\\b', 'a,b', '["a"]', 'a]b[', 'think:0:0', ''];
    const keys = ids.map((id) => compositeKey('thread-a', id));
    expect(new Set(keys).size).toBe(ids.length);
  });

  it('throws, naming the part, when a part carries the separator', () => {
    expect(() => compositeKey('thread-a', `pay${COMPOSITE_KEY_SEPARATOR}load`))
      .toThrow(/part 1/);
    expect(() => compositeKey(`a${COMPOSITE_KEY_SEPARATOR}b`, 'c'))
      .toThrow(/part 0/);
  });

  it('refuses a pre-joined sub-key rather than nesting it silently', () => {
    // The failure this exists to catch: a helper that joins its own parts
    // and hands the result over as ONE part. `['a', 'b\0c']` and
    // `['a\0b', 'c']` are different tuples with the same key, and the
    // symptom is one row reading another's expansion state.
    const subKey = compositeKey('b', 'c');
    expect(() => compositeKey('a', subKey)).toThrow(/separator/);
  });
});
