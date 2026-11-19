import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Token } from '../parser/engine';
import type { DestinationHook } from '../parser/engine/destination';
import { htmlRegionNodes, parseHtmlRegion, sanitizeEmbeddedHtmlToken, type SanitizeEmbeddedHtmlOptions } from './htmlSanitize';
import { htmlPolicyOf } from '../parser/extensions/htmlPolicy';

// The sanitizer's host hooks: `text` turns a text run into inline tokens,
// `destinations` decide an href or src, `anchors` draws same-document jumps.
// Whatever a hook returns renders under the allowlist and URL policy.

const block = (raw: string, options: SanitizeEmbeddedHtmlOptions) => sanitizeEmbeddedHtmlToken({ raw, block: true }, options);

const text = (value: string): Token => ({ type: 'text', raw: value, text: value });
const link = (href: string, shown: string): Token => ({ type: 'link', raw: shown, href, title: null, text: shown, tokens: [text(shown)] });

/** Links every `#N` in a run, as a forge's reference filter does. */
const issues: SanitizeEmbeddedHtmlOptions['text'] = (run, linkable) => {
  const parts = run.split(/(#\d+)/);
  if (parts.length === 1) return null;
  return parts.filter(Boolean).map((part) => (linkable && /^#\d+$/.test(part) ? link(`https://example.invalid/issues/${part.slice(1)}`, part) : text(part)));
};

describe('the text hook', () => {
  it('renders the tokens it returns in place of the run', () => {
    expect(block('<td>See #1, ok</td>', { text: issues })).toBe('See <a href="https://example.invalid/issues/1">#1</a>, ok');
    expect(block('<p>a <b>#2</b></p>', { text: issues })).toBe('<p>a <b><a href="https://example.invalid/issues/2">#2</a></b></p>');
  });

  it('is not asked inside code, pre, tt or samp, and links nothing inside an anchor or kbd', () => {
    const hook = vi.fn(issues);
    const out = block('<p><code>#1</code><pre>#2</pre><tt>#3</tt><samp>#4</samp><a href="https://example.invalid/">#5</a><kbd>#6</kbd></p>', { text: hook });
    expect(hook.mock.calls).toEqual([['#5', false], ['#6', false]]);
    expect(out).toContain('<a href="https://example.invalid/">#5</a>');
    expect(out).toContain('<kbd>#6</kbd>');
    expect(out).not.toContain('issues');
  });

  it('is asked once per text run of a block, never for an inline tag token', () => {
    const hook = vi.fn((_run: string, _linkable: boolean): Token[] | null => null);
    expect(block('<div>one<br>two</div>', { text: hook })).toBe('<div>one<br>two</div>');
    expect(hook.mock.calls.map(([run]) => run)).toEqual(['one', 'two']);
    hook.mockClear();
    sanitizeEmbeddedHtmlToken({ raw: '<textarea>', block: false }, { text: hook, policy: htmlPolicyOf('github') });
    expect(hook).not.toHaveBeenCalled();
  });

  it('cannot inject markup or a refused URL', () => {
    const hostile: SanitizeEmbeddedHtmlOptions['text'] = () => [
      text('<script>alert(1)</script>'),
      link('javascript:alert(1)', 'js'),
      link('docs/a.md', 'relative'),
      { type: 'html', raw: '<img src=x onerror=alert(1)>', text: '', block: false } as Token,
      { type: 'image', raw: 'i', href: 'https://example.invalid/i.png" onerror="x', text: 'alt', title: null, tokens: [] } as Token,
      { type: 'codespan', raw: '`<b>`', text: '<b>' },
    ];
    const out = block('<p>x</p>', { text: hostile });
    const doc = new DOMParser().parseFromString(out, 'text/html');
    expect([...doc.body.querySelectorAll('*')].map((element) => element.localName)).toEqual(['p', 'img', 'code']);
    expect(doc.body.querySelector('img')!.getAttribute('src')).toBe('https://example.invalid/i.png%22%20onerror=%22x');
    expect(doc.body.querySelector('img')!.getAttributeNames().sort()).toEqual(['alt', 'src']);
    expect(doc.body.textContent).toBe('<script>alert(1)</script>jsrelative<img src=x onerror=alert(1)><b>');
  });

  it('renders a returned image through the media claim', () => {
    const emoji: SanitizeEmbeddedHtmlOptions['text'] = () => [
      { type: 'image', raw: ':s:', href: '/uploads/s.png', text: ':s:', title: ':s:', tokens: [], width: '20', height: '20' } as Token,
    ];
    const out = block('<p>:s:</p>', {
      text: emoji,
      destinations: [(token) => (token.href === '/uploads/s.png' ? { ...token, href: 'claim:s' } : undefined)],
      allowedImagePrefixes: ['claim:'],
    });
    expect(out).toBe('<p><img data-markdown-media-claim="claim:s" alt=":s:" title=":s:" width="20" height="20"></p>');
  });
});

describe('the destination hooks', () => {
  // A relative link resolved, as the forge reference hook resolves one.
  const resolve = vi.fn<DestinationHook>((token) =>
    token.type !== 'link' || /^[a-z]+:/.test(token.href)
      ? undefined
      : { ...token, href: token.href === 'bad' ? 'javascript:alert(1)' : `https://example.invalid/base/${token.href}` },
  );
  beforeEach(() => {
    resolve.mockClear();
  });

  it('decide every href, and their answer passes the same policy', () => {
    expect(block('<a href="docs/a b.md">d</a><a href="bad">b</a><a href="https://example.invalid/x">x</a>', { destinations: [resolve] })).toBe(
      '<a href="https://example.invalid/base/docs/a%20b.md">d</a><a>b</a><a href="https://example.invalid/x">x</a>',
    );
    expect(resolve.mock.calls.map(([token]) => token.href)).toEqual(['docs/a b.md', 'bad', 'https://example.invalid/x']);
  });

  it('decide an image source as an image, which a link resolver leaves', () => {
    expect(block('<img src="i.png">', { destinations: [resolve] })).not.toContain('src=');
    expect(resolve.mock.calls.map(([token]) => token.type)).toEqual(['image']);
  });
});

describe('same-document jumps', () => {
  it('mark a fragment link and an anchor name, never an id or name', () => {
    const raw = '<a href="#x">to x</a><a name="x"></a><div id="y">y</div><a id="z"></a>';
    expect(block(raw, { anchors: { ids: false } })).toBe('<a data-md-fragment="" href="#x">to x</a><a data-md-anchor="x"></a><div>y</div><a></a>');
    expect(block(raw, { anchors: { ids: true } })).toBe(
      '<a data-md-fragment="" href="#x">to x</a><a data-md-anchor="x"></a><div data-md-anchor="y">y</div><a data-md-anchor="z"></a>',
    );
  });

  it('are dropped without the option, and an authored marker never passes', () => {
    expect(block('<a href="#x" data-md-fragment="">to x</a><a name="x" data-md-anchor="x"></a>', {})).toBe('<a>to x</a><a></a>');
  });

  it('stand on an html region element that holds an island', () => {
    const parsed = parseHtmlRegion(['<div id="top"><a href="#top">', {}, '</a></div>'], (token) =>
      sanitizeEmbeddedHtmlToken(token, { anchors: { ids: true } }),
    );
    const [div] = htmlRegionNodes(parsed, ['*']);
    expect(div).toMatchObject({ kind: 'element', tag: 'div', attrs: { 'data-md-anchor': 'top' } });
    const [anchor] = (div as { children: unknown[] }).children;
    expect(anchor).toMatchObject({ kind: 'element', tag: 'a', attrs: { 'data-md-fragment': '', href: '#top' } });
    expect(Object.keys((div as { attrs: object }).attrs)).not.toContain('id');
  });
});
