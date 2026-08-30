// First-divergence reporting for the markdown suites.
//
// These tests compare whole streamed documents and whole token trees:
// when one diverges, the default reporter prints both sides in full,
// multi-KB bodies of near-identical filler with the one differing
// character somewhere inside them. That is not a diff, it is a wall.
//
// `toEqualWithFirstDivergence` keeps `toEqual`'s exact semantics — it
// defers to the same deep-equality tester, so key order and asymmetric
// matchers behave identically — and only replaces the FAILURE MESSAGE
// with the index of the first differing character and a window of
// context either side.

import { expect } from 'vitest';

/** Characters of context shown on each side of the divergence point. */
export const DIVERGENCE_CONTEXT_CHARS = 80;

function serialize(value: unknown): string {
  if (typeof value === 'string') return value;
  try {
    return JSON.stringify(value) ?? String(value);
  } catch {
    // Cyclic or otherwise unserializable: fall back to a shape we can
    // still scan, rather than losing the report entirely.
    return String(value);
  }
}

export interface FirstDivergence {
  /** Index of the first differing code unit in the serialized forms. */
  index: number;
  /** Total serialized lengths, so a pure truncation is obvious. */
  receivedLength: number;
  expectedLength: number;
  receivedWindow: string;
  expectedWindow: string;
}

/**
 * First point at which two values' serialized forms differ, or null when
 * the serialized forms are identical (deep inequality that survives
 * serialization equality is a key-order or prototype difference, which
 * the caller reports on its own).
 */
export function findFirstDivergence(
  received: unknown,
  expected: unknown,
  context = DIVERGENCE_CONTEXT_CHARS,
): FirstDivergence | null {
  const receivedText = serialize(received);
  const expectedText = serialize(expected);
  const shared = Math.min(receivedText.length, expectedText.length);
  let index = 0;
  while (index < shared && receivedText[index] === expectedText[index]) index++;
  if (index === shared && receivedText.length === expectedText.length) return null;
  const from = Math.max(0, index - context);
  const to = index + context;
  return {
    index,
    receivedLength: receivedText.length,
    expectedLength: expectedText.length,
    receivedWindow: receivedText.slice(from, to),
    expectedWindow: expectedText.slice(from, to),
  };
}

/** Human-readable report, with `|` marking the divergence point. */
export function describeFirstDivergence(
  received: unknown,
  expected: unknown,
  context = DIVERGENCE_CONTEXT_CHARS,
): string {
  const divergence = findFirstDivergence(received, expected, context);
  if (!divergence) {
    return 'values are deeply unequal but serialize identically '
      + '(key order, prototype, or a non-enumerable difference)';
  }
  const marker = Math.min(divergence.index, context);
  const window = (text: string): string =>
    `${text.slice(0, marker)}|${text.slice(marker)}`;
  return [
    `first divergence at index ${divergence.index} of `
      + `${divergence.receivedLength} received / ${divergence.expectedLength} expected`,
    `expected …${window(divergence.expectedWindow)}…`,
    `received …${window(divergence.receivedWindow)}…`,
  ].join('\n');
}

expect.extend({
  toEqualWithFirstDivergence(received: unknown, expected: unknown) {
    // Same tester `toEqual` uses, so semantics are identical and only the
    // message changes.
    const pass = this.equals(received, expected, this.customTesters);
    return {
      pass,
      message: () =>
        pass
          ? 'expected values to differ, but they are deeply equal'
          : describeFirstDivergence(received, expected),
      // Suppress the reporter's own full-body dump: the message above IS
      // the diff, and printing megabytes beside it defeats the purpose.
      actual: undefined,
      expected: undefined,
    };
  },
});

declare module 'vitest' {
  interface Matchers<T = any> {
    /**
     * `toEqual`, but a failure reports the first differing character with
     * ~80 chars of context each side instead of dumping both bodies.
     * For whole-document and whole-token-tree comparisons.
     */
    toEqualWithFirstDivergence(expected: unknown): T;
  }
}
