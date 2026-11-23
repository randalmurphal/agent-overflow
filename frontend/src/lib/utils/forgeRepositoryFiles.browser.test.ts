import { describe, expect, it } from 'vitest';
import { markdownFindText } from '../components/chat/markdown/markdownFindText';
import type { ForgeAttachmentSource } from './forgeAttachments';
import { imageFactKey, REVISION_FACT_KEY, type FactRevision, type ForgeFactReader, type ForgeFactState } from './forgeFacts';
import { forgeFactKeys } from './forgeReferenceExtension';

// In real Chromium, whose HTML parser reads an attribute as the app's
// webviews do: the repository image scan names the key the sanitizer and
// the inline tag reader read for values only the HTML parser decides
// (happy-dom reads some of them otherwise).

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};
const HEAD = 'a'.repeat(40);
const REVISION: FactRevision = { head: HEAD, base: 'b'.repeat(40), headRef: 'feature/x', baseRef: 'main' };

function reader(): ForgeFactReader & { read: Set<string> } {
  const read = new Set<string>();
  const answer = (key: string): ForgeFactState => (key === REVISION_FACT_KEY ? { key, status: 'found', revision: REVISION } : 'pending');
  return { read, get: (key) => (read.add(key), answer(key)), signature: (key) => JSON.stringify(answer(key)) ?? '' };
}

describe('the repository image scan under a browser HTML parser', () => {
  it.each([
    ['a quote in an unquoted value', '<p><img src=docs/a"b.png></p>', 'docs/a"b.png'],
    ['a quote in an inline unquoted value', 'See <img src=docs/a\'b.png> here.', "docs/a'b.png"],
    ['a legacy reference before punctuation', '<p><img src="docs/a&amp.png"></p>', 'docs/a&.png'],
    ['a legacy reference before a letter', '<p><img src="docs/a&ampx.png"></p>', 'docs/a&ampx.png'],
    ['a named reference', '<p><img src="docs/a&period;png"></p>', 'docs/a.png'],
    ['a reference in a srcset', '<picture><source media="(prefers-color-scheme: dark)" srcset="docs/d&lowbar;k.png 1x"><img src="docs/l.png"></picture>', 'docs/d_k.png'],
  ])('names the key read for %s', (_name, body, path) => {
    const facts = reader();
    markdownFindText(body, { embeddedHtml: true, forgeSource: GITHUB, facts: () => facts });
    const key = imageFactKey(HEAD, path);
    expect([...facts.read].filter((each) => each.startsWith('image:'))).toContain(key);
    expect(forgeFactKeys(body, GITHUB, REVISION)).toContain(key);
  });
});
