import { describe, expect, it } from 'vitest';
import {
  registerFootnoteSource,
  resolveFootnoteBody,
} from './footnoteDefinitions';

// The lookup exists because the renderer cannot answer it: a `[^1]: body`
// definition is always its own block, blocks are lexed in isolation, and
// the definition token never reaches the DOM.
// What is pinned here is that the answer comes from the real grammar — a
// `[^x]:` line inside a fence is code, a definition inside a list item is
// still a definition — and that the registry is scoped to the surface that
// rendered the chip.

function surface(source: string, label: string): HTMLElement {
  const root = document.createElement('div');
  root.className = 'markdown-body';
  const chip = document.createElement('button');
  chip.dataset.footnoteLabel = label;
  root.appendChild(chip);
  document.body.appendChild(root);
  registerFootnoteSource(root, () => source);
  return chip;
}

describe('resolveFootnoteBody', () => {
  it('resolves a definition that follows its reference', () => {
    const chip = surface('A claim[^note].\n\n[^note]: The body.', 'note');
    expect(resolveFootnoteBody(chip)).toBe('The body.');
  });

  it('resolves a definition that precedes its reference', () => {
    const chip = surface('[^note]: The body.\n\nA claim[^note].', 'note');
    expect(resolveFootnoteBody(chip)).toBe('The body.');
  });

  it('keeps the definition body as markdown, multi-line and all', () => {
    const chip = surface(
      'A claim[^n].\n\n[^n]: See **weighted** `avg()`\n    over the window.',
      'n',
    );
    expect(resolveFootnoteBody(chip)).toBe(
      'See **weighted** `avg()`\nover the window.',
    );
  });

  it('does not read a definition-shaped line out of a fenced code block', () => {
    // The pre-lex veto is a textual test and lets this through; the lex
    // behind it is the authority, and it sees code.
    const chip = surface(
      'A claim[^note].\n\n```md\n[^note]: not a definition\n```',
      'note',
    );
    expect(resolveFootnoteBody(chip)).toBeNull();
  });

  it('returns null for a label the document never defines', () => {
    const chip = surface('A dangling claim[^missing].', 'missing');
    expect(resolveFootnoteBody(chip)).toBeNull();
  });

  it('returns null for a document with no footnotes at all', () => {
    const chip = surface('Just prose.', 'note');
    expect(resolveFootnoteBody(chip)).toBeNull();
  });

  it('serves every reference to one label from the same definition', () => {
    const source = 'First[^a] and second[^a].\n\n[^a]: Shared body.';
    const one = surface(source, 'a');
    const two = surface(source, 'a');
    expect(resolveFootnoteBody(one)).toBe('Shared body.');
    expect(resolveFootnoteBody(two)).toBe('Shared body.');
  });

  it('scopes the lookup to the surface the chip was rendered in', () => {
    // Two messages, each with its own `[^1]`. A chip must never resolve
    // against a neighbour's definitions.
    const first = surface('One[^1].\n\n[^1]: First body.', '1');
    const second = surface('Two[^1].\n\n[^1]: Second body.', '1');
    expect(resolveFootnoteBody(first)).toBe('First body.');
    expect(resolveFootnoteBody(second)).toBe('Second body.');
    // And back again: the memo is keyed on the source, so alternating
    // surfaces re-lexes rather than serving a stale map.
    expect(resolveFootnoteBody(first)).toBe('First body.');
  });

  it('resolves nothing for a chip with no registered surface', () => {
    const orphan = document.createElement('button');
    orphan.dataset.footnoteLabel = 'note';
    document.body.appendChild(orphan);
    expect(resolveFootnoteBody(orphan)).toBeNull();
  });

  it('stops resolving once the surface unregisters', () => {
    const root = document.createElement('div');
    root.className = 'markdown-body';
    const chip = document.createElement('button');
    chip.dataset.footnoteLabel = 'note';
    root.appendChild(chip);
    document.body.appendChild(root);

    const release = registerFootnoteSource(
      root,
      () => 'A claim[^note].\n\n[^note]: The body.',
    );
    expect(resolveFootnoteBody(chip)).toBe('The body.');

    release();
    expect(resolveFootnoteBody(chip)).toBeNull();
  });

  it('reads the source lazily, so a streaming surface resolves against its latest text', () => {
    const root = document.createElement('div');
    root.className = 'markdown-body';
    const chip = document.createElement('button');
    chip.dataset.footnoteLabel = 'note';
    root.appendChild(chip);
    document.body.appendChild(root);

    let source = 'A claim[^note].';
    registerFootnoteSource(root, () => source);
    expect(resolveFootnoteBody(chip)).toBeNull();

    source = 'A claim[^note].\n\n[^note]: Arrived later.';
    expect(resolveFootnoteBody(chip)).toBe('Arrived later.');
  });
});
