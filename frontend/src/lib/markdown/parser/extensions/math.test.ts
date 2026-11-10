// The currency check that keeps `$199, 299$` out of the math path.
import { describe, expect, it } from 'vitest';
import { lex } from '../../index';

type Node = { type: string; tokens?: Node[] };

function hasMath(source: string): boolean {
  const walk = (nodes: Node[]): boolean =>
    nodes.some((node) => node.type === 'math' || walk(node.tokens ?? []));
  return walk(lex(source) as Node[]);
}

describe('single-dollar currency detection', () => {
  it('reads a list of numbers between dollars as prices, not math', () => {
    expect(hasMath('from $199, 299$ each')).toBe(false);
    expect(hasMath('from $1.50; 2.75$ each')).toBe(false);
    expect(hasMath('from $1,234,567$ each')).toBe(false);
  });

  it('still reads an expression between dollars as math', () => {
    expect(hasMath('so $a, b$ holds')).toBe(true);
    expect(hasMath('so $1.5x$ holds')).toBe(true);
  });

  // A near miss of the number-list shape. A pattern in which a comma can
  // either continue a number or start the next one tries every split of the
  // commas before giving up: this input never finishes under it.
  it('rejects a long near miss of a number list without backtracking', () => {
    const source = `so $1${',1'.repeat(200)}x$ holds`;
    expect(hasMath(source)).toBe(true);
  });
});
