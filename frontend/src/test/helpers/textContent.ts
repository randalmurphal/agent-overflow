// Whitespace-normalized text of a node, as jest-dom's `toHaveTextContent`
// compares it. Unit tests match it with `toMatch` for RegExp checks:
// Vitest 5 types `toHaveTextContent` with browser mode's exact string-only
// signature across the whole program, so jest-dom's RegExp form no longer
// type-checks in the happy-dom project even though it still runs there.
export function normalizedTextContent(node: Node | null): string {
  if (!node) throw new Error('expected a node, received null');
  return (node.textContent ?? '').replace(/\s+/g, ' ').trim();
}
