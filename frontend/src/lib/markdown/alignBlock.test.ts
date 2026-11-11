import { describe, expect, it } from 'vitest';
import { parseIncompleteMarkdown } from './parser/incompleteMarkdown';

describe('incomplete alignment blocks', () => {
  it.each([
    ['[center]\nwords\n[', '[center]\nwords\n[/center]'],
    ['[center]\nwords\n[/cen', '[center]\nwords\n[/center]'],
    ['[center]\nwords\n[/center', '[center]\nwords\n[/center]'],
    ['[right]\nwords\n[/rig', '[right]\nwords\n[/right]'],
    // Not a prefix of the closer: content, which the link completer closes.
    ['[center]\nwords\n[/x', '[center]\nwords\n[/x]\n[/center]'],
    ['[center]\nwords\n[/right', '[center]\nwords\n[/right]\n[/center]'],
    // The sealed opener is never dropped.
    ['[center]\n[', '[center]\n[/center]'],
    ['[center]\n', '[center]\n\n[/center]'],
    // A bare trailing opener is left as written.
    ['[center]', '[center]'],
  ])('seals %j as %j', (source, sealed) => {
    expect(parseIncompleteMarkdown(source)).toBe(sealed);
  });
});
