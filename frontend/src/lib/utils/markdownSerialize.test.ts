import { afterEach, describe, expect, it } from 'vitest';
import { serializeRangeToMarkdown } from './markdownSerialize';

// Range over the entire content of an element. Used by most of the
// per-shape tests below — they construct a DOM that mirrors what
// marked emits and verify the round-trip out the other side.
function selectAll(host: HTMLElement): Range {
  const range = document.createRange();
  range.selectNodeContents(host);
  return range;
}

function asMarkdownBody(html: string): HTMLElement {
  const host = document.createElement('div');
  host.className = 'markdown-body';
  host.innerHTML = html;
  document.body.appendChild(host);
  return host;
}

afterEach(() => {
  document.body.innerHTML = '';
});

describe('serializeRangeToMarkdown — inline', () => {
  it('preserves bold markers', () => {
    const host = asMarkdownBody('<p>Hello <strong>world</strong> ok</p>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('Hello **world** ok');
  });

  it('preserves italic markers', () => {
    const host = asMarkdownBody('<p>a <em>b</em> c</p>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('a *b* c');
  });

  it('preserves strikethrough markers', () => {
    const host = asMarkdownBody('<p>a <del>b</del> c</p>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('a ~~b~~ c');
  });

  it('preserves inline code with backticks', () => {
    const host = asMarkdownBody('<p>see <code>foo()</code> for details</p>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('see `foo()` for details');
  });

  it('escalates the inline-code fence past any embedded backticks', () => {
    const host = asMarkdownBody('<p><code>a `b` c</code></p>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('``a `b` c``');
  });

  it('pads inline code that begins or ends with a backtick', () => {
    const host = asMarkdownBody('<p><code>`tick</code></p>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('`` `tick ``');
  });

  it('emits regular links as [text](href)', () => {
    const host = asMarkdownBody('<p><a href="https://x.example">label</a></p>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('[label](https://x.example)');
  });

  it('emits path-link enrichment (.editor-link) as plain text', () => {
    // The path linkifier produces href="#" + data-path; rendering
    // those as `[text](#)` would pollute the clipboard with a dummy
    // anchor, so the serializer collapses them to text.
    const host = asMarkdownBody(
      '<p>see <a class="editor-link" href="#" data-path="src/x.ts">src/x.ts</a></p>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('see src/x.ts');
  });

  it('emits <br> as a markdown hard break', () => {
    const host = asMarkdownBody('<p>line one<br>line two</p>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('line one  \nline two');
  });

  it('emits images as ![alt](src)', () => {
    const host = asMarkdownBody('<p><img alt="Logo" src="https://x/logo.png"></p>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('![Logo](https://x/logo.png)');
  });

  it('escapes brackets in link text so a smuggled bracket cannot forge a fake link', () => {
    // Without escaping, the rendered visible text `foo](evil` would
    // re-tokenize as `[foo](evil)baz](https://safe)` — pasted into
    // any markdown renderer that's a link to `evil`, not the
    // visible-href `https://safe`. The escaping kills that vector.
    const host = document.createElement('div');
    host.className = 'markdown-body';
    const a = document.createElement('a');
    a.href = 'https://safe';
    a.textContent = 'foo](evil)baz';
    host.appendChild(a);
    document.body.appendChild(host);
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '[foo\\]\\(evil\\)baz](https://safe)',
    );
  });

  it('escapes parens in href so embedded parens stay inside the URL', () => {
    const host = document.createElement('div');
    host.className = 'markdown-body';
    const a = document.createElement('a');
    a.setAttribute('href', 'https://x.example/path(weird)stuff');
    a.textContent = 'click';
    host.appendChild(a);
    document.body.appendChild(host);
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '[click](https://x.example/path\\(weird\\)stuff)',
    );
  });

  it('escapes brackets in image alt text', () => {
    const host = asMarkdownBody(
      '<p><img alt="x](evil" src="https://x/y.png"></p>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '![x\\]\\(evil](https://x/y.png)',
    );
  });

  it('drops img src that is data: but not an image MIME type, leaving alt as text', () => {
    // DOMPurify already filters most schemes upstream, but the
    // serializer adds defense-in-depth: a data:text/html bypass would
    // otherwise round-trip into the clipboard verbatim.
    const host = asMarkdownBody(
      '<p><img alt="hi" src="data:text/html,<script>alert(1)</script>"></p>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('hi');
  });

  it('keeps img src for legitimate data:image URIs', () => {
    const host = asMarkdownBody(
      '<p><img alt="px" src="data:image/png;base64,iVBORw0K"></p>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '![px](data:image/png;base64,iVBORw0K)',
    );
  });
});

describe('serializeRangeToMarkdown — blocks', () => {
  it('emits headings with the correct level', () => {
    for (const level of [1, 2, 3, 4, 5, 6]) {
      const host = asMarkdownBody(`<h${level}>Title</h${level}>`);
      expect(serializeRangeToMarkdown(selectAll(host))).toBe(`${'#'.repeat(level)} Title`);
    }
  });

  it('emits blockquotes with > prefix per line', () => {
    const host = asMarkdownBody('<blockquote><p>line one</p><p>line two</p></blockquote>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('> line one\n>\n> line two');
  });

  it('emits horizontal rules', () => {
    const host = asMarkdownBody('<p>before</p><hr><p>after</p>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('before\n\n---\n\nafter');
  });

  it('separates consecutive paragraphs with a blank line', () => {
    const host = asMarkdownBody('<p>one</p><p>two</p>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('one\n\ntwo');
  });
});

describe('serializeRangeToMarkdown — lists', () => {
  it('emits a tight ordered list with sequential numbers', () => {
    const host = asMarkdownBody('<ol><li>foo</li><li>bar</li><li>baz</li></ol>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('1. foo\n2. bar\n3. baz');
  });

  it('honors the ordered list `start` attribute', () => {
    const host = asMarkdownBody('<ol start="5"><li>foo</li><li>bar</li></ol>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('5. foo\n6. bar');
  });

  it('preserves start="0" instead of silently bumping it to 1', () => {
    // `Number(s) || 1` would short-circuit on the falsy 0; the
    // explicit Number.isFinite check fixes that.
    const host = asMarkdownBody('<ol start="0"><li>zero</li><li>one</li></ol>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('0. zero\n1. one');
  });

  it('honors a multi-digit start value', () => {
    const host = asMarkdownBody('<ol start="10"><li>ten</li><li>eleven</li></ol>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('10. ten\n11. eleven');
  });

  it('falls back to start=1 when the start attribute is non-numeric', () => {
    const host = asMarkdownBody('<ol start="oops"><li>foo</li></ol>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('1. foo');
  });

  it('emits a tight unordered list with - markers', () => {
    const host = asMarkdownBody('<ul><li>foo</li><li>bar</li></ul>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('- foo\n- bar');
  });

  it('indents nested unordered list items by two spaces', () => {
    const host = asMarkdownBody(
      '<ul><li>a<ul><li>b</li></ul></li><li>c</li></ul>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('- a\n  - b\n- c');
  });

  it('indents a ul nested under an ol by three spaces (matches "1. " width)', () => {
    const host = asMarkdownBody(
      '<ol><li>foo<ul><li>bar</li></ul></li><li>baz</li></ol>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '1. foo\n   - bar\n2. baz',
    );
  });

  it('preserves multiple paragraphs inside a single list item', () => {
    const host = asMarkdownBody('<ul><li><p>foo</p><p>bar</p></li></ul>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('- foo\n\n  bar');
  });

  it('preserves inline formatting inside list items', () => {
    const host = asMarkdownBody(
      '<ul><li><strong>name</strong>: value</li></ul>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('- **name**: value');
  });
});

describe('serializeRangeToMarkdown — code blocks', () => {
  it('emits language-tagged fences', () => {
    const host = asMarkdownBody(
      '<pre><code class="language-typescript">const x = 1;</code></pre>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '```typescript\nconst x = 1;\n```',
    );
  });

  it('omits language when the class is missing', () => {
    const host = asMarkdownBody('<pre><code>raw\ntext</code></pre>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('```\nraw\ntext\n```');
  });

  it('strips per-pre CopyButton mounts from output', () => {
    // markdownEnhance injects [data-code-copy-mount] inside <pre>.
    // The serializer must skip that chrome.
    const host = asMarkdownBody(
      '<pre><code class="language-go">fmt.Println("hi")</code><div data-code-copy-mount="true">copy</div></pre>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '```go\nfmt.Println("hi")\n```',
    );
  });

  it('walks Shiki token spans via textContent', () => {
    // Shiki replaces the inner <code> with <span class="line"><span style>...
    // tokens. textContent walks through them as if they were a flat
    // string, so the serializer doesn't need to know about Shiki.
    const host = asMarkdownBody(
      '<pre class="shiki"><code class="language-typescript">' +
        '<span class="line"><span style="color:#abc">const</span><span> x</span></span>' +
        '<span class="line"><span style="color:#abc"> = 1;</span></span>' +
      '</code></pre>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '```typescript\nconst x = 1;\n```',
    );
  });
});

describe('serializeRangeToMarkdown — math and mermaid', () => {
  it('uses data-math-source for inline math (KaTeX wipes the original textContent)', () => {
    const host = asMarkdownBody(
      '<p>see <span class="math-inline" data-math-source="x^2"><span class="katex">RENDERED</span></span></p>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('see $x^2$');
  });

  it('uses data-math-source for display math', () => {
    const host = asMarkdownBody(
      '<div class="math-display" data-math-source="\\sum_{i=1}^n i"><span class="katex">RENDERED</span></div>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('$$\n\\sum_{i=1}^n i\n$$');
  });

  it('ignores a forged data-math-source when KaTeX has not rendered into the node', () => {
    // Without rendered .katex evidence, the data attribute is not
    // trustworthy — copy must reflect what the user can see.
    const host = asMarkdownBody(
      '<p><span class="math-inline" data-math-source="$(rm -rf ~)">harmless</span></p>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('$harmless$');
  });

  it('reads mermaid source from data-mermaid-source on the rendered <pre>', () => {
    const host = asMarkdownBody(
      '<pre class="mermaid mermaid-rendered" data-mermaid-source="graph TD\nA --> B"><svg></svg></pre>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '```mermaid\ngraph TD\nA --> B\n```',
    );
  });

  it('ignores a forged data-mermaid-source on a <pre> with no rendered SVG', () => {
    // Same threat model as math: the data attribute is only
    // authoritative when the renderer has rewritten the DOM.
    const host = asMarkdownBody(
      '<pre data-mermaid-source="evil source"><code>visible code</code></pre>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '```\nvisible code\n```',
    );
  });
});

describe('serializeRangeToMarkdown — tables', () => {
  it('emits a GFM table with header separator', () => {
    const host = asMarkdownBody(
      '<table><thead><tr><th>name</th><th>age</th></tr></thead>' +
      '<tbody><tr><td>Ada</td><td>36</td></tr><tr><td>Lin</td><td>27</td></tr></tbody></table>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      [
        '| name | age |',
        '| --- | --- |',
        '| Ada | 36 |',
        '| Lin | 27 |',
      ].join('\n'),
    );
  });

  it('respects column alignment from inline style', () => {
    const host = asMarkdownBody(
      '<table><thead><tr><th style="text-align:left">a</th>' +
      '<th style="text-align:center">b</th>' +
      '<th style="text-align:right">c</th></tr></thead>' +
      '<tbody><tr><td>1</td><td>2</td><td>3</td></tr></tbody></table>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      [
        '| a | b | c |',
        '| :--- | :---: | ---: |',
        '| 1 | 2 | 3 |',
      ].join('\n'),
    );
  });

  it('escapes pipes inside cell content', () => {
    const host = asMarkdownBody(
      '<table><thead><tr><th>op</th></tr></thead><tbody><tr><td>a | b</td></tr></tbody></table>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '| op |\n| --- |\n| a \\| b |',
    );
  });

  it('emits body rows for a table that has no <thead>', () => {
    // Marked usually emits a thead; a hand-rolled HTML table might
    // not. Don't drop the body when the header is missing.
    const host = asMarkdownBody(
      '<table><tbody><tr><td>a</td><td>b</td></tr><tr><td>c</td><td>d</td></tr></tbody></table>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '| a | b |\n| c | d |',
    );
  });
});

describe('serializeRangeToMarkdown — nested blocks', () => {
  it('preserves a code block nested inside a blockquote', () => {
    const host = asMarkdownBody(
      '<blockquote><p>before</p><pre><code class="language-js">a()</code></pre></blockquote>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '> before\n>\n> ```js\n> a()\n> ```',
    );
  });

  it('preserves a blockquote nested inside a list item', () => {
    const host = asMarkdownBody(
      '<ul><li><blockquote><p>note</p></blockquote></li></ul>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe('- > note');
  });
});

describe('serializeRangeToMarkdown — partial selections', () => {
  it('returns null for a collapsed range', () => {
    const host = asMarkdownBody('<p>hello</p>');
    const range = document.createRange();
    range.setStart(host, 0);
    range.collapse(true);
    expect(serializeRangeToMarkdown(range)).toBeNull();
  });

  it('returns null for a selection that yields no visible content', () => {
    // Selection over a copy-button mount alone — chrome only.
    const host = asMarkdownBody('<p><span data-code-copy-mount="true">btn</span></p>');
    expect(serializeRangeToMarkdown(selectAll(host))).toBeNull();
  });

  it('drops inline wrappers when the selection sits entirely inside a single text node', () => {
    // Per the DOM spec, Range.cloneContents over a range whose start
    // and end share a text node returns just the partial text — no
    // ancestor wrappers — so a within-<strong> selection produces
    // unstyled text. This matches the "select what you see" feel of
    // Notion/Linear: highlighting "ol" from <strong>bold</strong>
    // gives "ol", not "**ol**". To grab the markers, the user
    // extends the selection past the wrapper boundary (verified by
    // the cross-boundary test below).
    const host = asMarkdownBody('<p>before <strong>bold</strong> after</p>');
    const strong = host.querySelector('strong')!;
    const text = strong.firstChild as Text;
    const range = document.createRange();
    range.setStart(text, 1);
    range.setEnd(text, 3);
    expect(serializeRangeToMarkdown(range)).toBe('ol');
  });

  it('keeps the inline wrapper when the selection crosses its boundary', () => {
    // Now the range starts before <strong> and ends inside its text.
    // cloneContents includes a partial <strong>, so the serializer
    // emits the markers wrapping the inside text.
    const host = asMarkdownBody('<p>before <strong>bold</strong> after</p>');
    const p = host.querySelector('p')!;
    const beforeText = p.firstChild as Text;
    const strongText = (p.querySelector('strong') as HTMLElement).firstChild as Text;
    const range = document.createRange();
    range.setStart(beforeText, 'before '.length);
    range.setEnd(strongText, 3);
    expect(serializeRangeToMarkdown(range)).toBe('**bol**');
  });

  it('handles a selection that crosses paragraph boundaries', () => {
    const host = asMarkdownBody('<p>first paragraph</p><p>second paragraph</p>');
    const firstText = host.querySelector('p:first-child')!.firstChild as Text;
    const secondText = host.querySelector('p:nth-child(2)')!.firstChild as Text;
    const range = document.createRange();
    range.setStart(firstText, 'first '.length);
    range.setEnd(secondText, 'second '.length);
    // The trailing space inside "second " is part of the selection;
    // the serializer preserves it rather than guessing the user
    // wanted to trim.
    expect(serializeRangeToMarkdown(range)).toBe('paragraph\n\nsecond ');
  });
});
