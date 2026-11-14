import { getContext, setContext, type Snippet } from 'svelte';
import type { Tokens } from '../../../markdown';

// A host's claim on some of a document's fenced code blocks: ChatMarkdown
// renders a claimed block with the host's snippet in the block's place,
// inside the one Markdown document, and every other block as code. The
// review pane claims suggestion fences this way (a suggestion's diff
// where its fence is). A claimed block never serializes to static HTML,
// so the host's snippet stays mounted.

export interface CodeClaim {
  claims(token: Tokens.Code): boolean;
  render: Snippet<[Tokens.Code]>;
}

const CODE_CLAIM = Symbol('code-claim');

/** ChatMarkdown publishes its claim (read live) to its code islands. */
export function provideCodeClaim(read: () => CodeClaim | null): void {
  setContext(CODE_CLAIM, read);
}

export function readCodeClaim(): (() => CodeClaim | null) | undefined {
  return getContext<(() => CodeClaim | null) | undefined>(CODE_CLAIM);
}
