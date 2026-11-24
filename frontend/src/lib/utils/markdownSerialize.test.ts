import { afterEach, describe, expect, it } from 'vitest';
import { serializeRangeToMarkdown } from './markdownSerialize';
import { lex } from '../markdown';
import { PATH_LINK_HREF_PREFIX } from './pathLinkExtension';
import { buildForgeAttachmentHref } from './forgeAttachments';

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
  it.each<[string, string, string]>([
    ['preserves bold markers', '<p>Hello <strong>world</strong> ok</p>', 'Hello **world** ok'],
    ['preserves italic markers', '<p>a <em>b</em> c</p>', 'a *b* c'],
    ['preserves strikethrough markers', '<p>a <del>b</del> c</p>', 'a ~~b~~ c'],
    ['preserves inline code with backticks', '<p>see <code>foo()</code> for details</p>', 'see `foo()` for details'],
    ['escalates the inline-code fence past any embedded backticks', '<p><code>a `b` c</code></p>', '``a `b` c``'],
    ['pads inline code that begins or ends with a backtick', '<p><code>`tick</code></p>', '`` `tick ``'],
    [
      'emits regular links as [text](href)',
      '<p><a href="https://x.example">label</a></p>',
      '[label](https://x.example)',
    ],
    // Path links carry our custom nonce-prefixed scheme; rendering them
    // as `[text](agent-overflow:open?…)` would smuggle the internal
    // scheme into the clipboard, so the serializer collapses them to
    // text. The nonce is per-page-load so the test composes the prefix
    // from the same constant the serializer consumes.
    [
      'emits path-link anchors (agent-overflow:open) as plain text',
      `<p>see <a href="${PATH_LINK_HREF_PREFIX}path=src%2Fx.ts">src/x.ts</a></p>`,
      'see src/x.ts',
    ],
    // A rewritten markdown link ([the draft](~/notes.md)) has visible
    // text that is NOT the path. Collapsing it to plain text (the prose
    // rule above) would silently lose the destination on copy; the
    // serializer re-emits it as a markdown link targeting the PLAIN
    // path — never the internal scheme.
    [
      're-emits a LABELED path-link anchor as [label](path) so copy keeps the destination',
      `<p>see <a href="${PATH_LINK_HREF_PREFIX}path=%7E%2Fnotes.md&line=12">the draft</a></p>`,
      'see [the draft](~/notes.md:12)',
    ],
    // Wrapped-form path links render as `<a><code>src/x.ts</code></a>`.
    // The serializer must walk into the `<code>` child via
    // serializeChildren so the clipboard receives `` `src/x.ts` ``,
    // matching the input markdown — not the path-link href, not the
    // bare path text. This is the round-trip contract for the wrapped
    // branch of the marked extension.
    [
      'round-trips a path-link anchor wrapping a codespan back to `path` markdown',
      `<p>see <a href="${PATH_LINK_HREF_PREFIX}path=src%2Fx.ts"><code>src/x.ts</code></a> here</p>`,
      'see `src/x.ts` here',
    ],
    ['emits <br> as a markdown hard break', '<p>line one<br>line two</p>', 'line one  \nline two'],
    ['emits images as ![alt](src)', '<p><img alt="Logo" src="https://x/logo.png"></p>', '![Logo](https://x/logo.png)'],
    [
      'preserves a local image file URI instead of copying its blob URL',
      '<p><img alt="diagram" src="blob:page-local" data-markdown-image-src="file:///workspace/diagram.png"></p>',
      '![diagram](file:///workspace/diagram.png)',
    ],
    [
      'escapes brackets in image alt text',
      '<p><img alt="x](evil" src="https://x/y.png"></p>',
      '![x\\]\\(evil](https://x/y.png)',
    ],
    // DOMPurify already filters most schemes upstream, but the
    // serializer adds defense-in-depth: a data:text/html bypass would
    // otherwise round-trip into the clipboard verbatim.
    [
      'drops img src that is data: but not an image MIME type, leaving alt as text',
      '<p><img alt="hi" src="data:text/html,<script>alert(1)</script>"></p>',
      'hi',
    ],
    [
      'keeps img src for legitimate data:image URIs',
      '<p><img alt="px" src="data:image/png;base64,iVBORw0K"></p>',
      '![px](data:image/png;base64,iVBORw0K)',
    ],
  ])('%s', (_name, html, expected) => {
    expect(serializeRangeToMarkdown(selectAll(asMarkdownBody(html)))).toBe(expected);
  });

  // A forge attachment's anchor href names THIS page's PR, computer and
  // nonce. Pasted anywhere else it means nothing, while the markdown the
  // reader selected said `/uploads/<hex>/report.pdf`.
  it('restores the forge href a link and an image were written with', () => {
    const forgeHref = buildForgeAttachmentHref({
      href: '/uploads/0123456789abcdef0123456789abcdef/report.pdf',
      pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
      backend: 'gpu',
      webBase: 'https://gitlab.example.test/group/widget/-/merge_requests/3',
    });
    const host = asMarkdownBody(
      `<p><a href="${forgeHref}">the report</a> `
      + '<img alt="a shot" src="blob:page-local" '
      + 'data-markdown-image-src="/uploads/0123456789abcdef0123456789abcdef/shot.png"></p>',
    );
    expect(serializeRangeToMarkdown(selectAll(host))).toBe(
      '[the report](/uploads/0123456789abcdef0123456789abcdef/report.pdf) '
      + '![a shot](/uploads/0123456789abcdef0123456789abcdef/shot.png)',
    );
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
});

describe('serializeRangeToMarkdown — blocks', () => {
  it.each<[string, string, string]>([
    [
      'emits blockquotes with > prefix per line',
      '<blockquote><p>line one</p><p>line two</p></blockquote>',
      '> line one\n>\n> line two',
    ],
    ['emits horizontal rules', '<p>before</p><hr><p>after</p>', 'before\n\n---\n\nafter'],
    ['separates consecutive paragraphs with a blank line', '<p>one</p><p>two</p>', 'one\n\ntwo'],
  ])('%s', (_name, html, expected) => {
    expect(serializeRangeToMarkdown(selectAll(asMarkdownBody(html)))).toBe(expected);
  });

  it('emits headings with the correct level', () => {
    for (const level of [1, 2, 3, 4, 5, 6]) {
      const host = asMarkdownBody(`<h${level}>Title</h${level}>`);
      expect(serializeRangeToMarkdown(selectAll(host))).toBe(`${'#'.repeat(level)} Title`);
    }
  });
});

describe('serializeRangeToMarkdown — lists', () => {
  it.each<[string, string, string]>([
    [
      'emits a tight ordered list with sequential numbers',
      '<ol><li>foo</li><li>bar</li><li>baz</li></ol>',
      '1. foo\n2. bar\n3. baz',
    ],
    ['honors the ordered list `start` attribute', '<ol start="5"><li>foo</li><li>bar</li></ol>', '5. foo\n6. bar'],
    // `Number(s) || 1` would short-circuit on the falsy 0; the
    // explicit Number.isFinite check fixes that.
    [
      'preserves start="0" instead of silently bumping it to 1',
      '<ol start="0"><li>zero</li><li>one</li></ol>',
      '0. zero\n1. one',
    ],
    ['honors a multi-digit start value', '<ol start="10"><li>ten</li><li>eleven</li></ol>', '10. ten\n11. eleven'],
    ['falls back to start=1 when the start attribute is non-numeric', '<ol start="oops"><li>foo</li></ol>', '1. foo'],
    ['emits a tight unordered list with - markers', '<ul><li>foo</li><li>bar</li></ul>', '- foo\n- bar'],
    [
      'indents nested unordered list items by two spaces',
      '<ul><li>a<ul><li>b</li></ul></li><li>c</li></ul>',
      '- a\n  - b\n- c',
    ],
    [
      'indents a ul nested under an ol by three spaces (matches "1. " width)',
      '<ol><li>foo<ul><li>bar</li></ul></li><li>baz</li></ol>',
      '1. foo\n   - bar\n2. baz',
    ],
    [
      'preserves multiple paragraphs inside a single list item',
      '<ul><li><p>foo</p><p>bar</p></li></ul>',
      '- foo\n\n  bar',
    ],
    [
      'preserves inline formatting inside list items',
      '<ul><li><strong>name</strong>: value</li></ul>',
      '- **name**: value',
    ],
  ])('%s', (_name, html, expected) => {
    expect(serializeRangeToMarkdown(selectAll(asMarkdownBody(html)))).toBe(expected);
  });
});

// GFM task lists render the checked state as a disabled <input>, so it lives
// nowhere in the visible text: the previous walker copied `- [x] done` as a
// plain `- done`, silently flipping every checked box off for the reader
// pasting a checklist somewhere else.
describe('serializeRangeToMarkdown — task lists', () => {
  // The property path — the renderer binds `checked` as a DOM property
  // and never writes the attribute — is proved in
  // markdownSerialize.browser.test.ts: preserving checkedness across
  // Range.cloneContents is the HTML cloning steps' job, and happy-dom does
  // not implement them.
  it.each<[string, string, string]>([
    [
      'emits [x] / [ ] for checked and unchecked items',
      '<ul>'
      + '<li><input type="checkbox" disabled checked>done</li>'
      + '<li><input type="checkbox" disabled>todo</li>'
      + '</ul>',
      '- [x] done\n- [ ] todo',
    ],
    [
      'keeps the task marker inside an ordered list item',
      '<ol><li><input type="checkbox" disabled checked>ship it</li></ol>',
      '1. [x] ship it',
    ],
    [
      'indents continuation lines under the list marker, not the checkbox',
      '<ul><li><input type="checkbox" disabled><p>todo</p><p>why</p></li></ul>',
      '- [ ] todo\n\n  why',
    ],
    [
      'carries the marker into a nested task list',
      '<ul><li><input type="checkbox" disabled checked>parent'
      + '<ul><li><input type="checkbox" disabled>child</li></ul></li></ul>',
      '- [x] parent\n  - [ ] child',
    ],
    ['leaves a plain list item unmarked', '<ul><li>plain</li></ul>', '- plain'],
  ])('%s', (_name, html, expected) => {
    expect(serializeRangeToMarkdown(selectAll(asMarkdownBody(html)))).toBe(expected);
  });

  it('does not emit a stray marker for a checkbox selected on its own', () => {
    // A drag-select that clips the item's text still clones the input; the
    // <li> is out of the fragment, so nothing owns the marker.
    const host = asMarkdownBody(
      '<ul><li><input type="checkbox" disabled checked>done</li></ul>',
    );
    const range = document.createRange();
    range.selectNode(host.querySelector('input')!);
    expect(serializeRangeToMarkdown(range)).toBeNull();
  });
});

describe('serializeRangeToMarkdown — code blocks', () => {
  it.each<[string, string, string]>([
    [
      'emits language-tagged fences',
      '<pre><code class="language-typescript">const x = 1;</code></pre>',
      '```typescript\nconst x = 1;\n```',
    ],
    [
      'reads the info string from the production source-free code host',
      '<div data-code-source="" data-code-lang="typescript title=demo">' +
        '<div><pre><code>const x = 1;</code></pre></div>' +
      '</div>',
      '```typescript title=demo\nconst x = 1;\n```',
    ],
    [
      'does not trust structural bytes in a foreign code-host info string',
      '<div data-code-lang="bad&#10;```"><pre><code class="language-ts">x</code></pre></div>',
      '```ts\nx\n```',
    ],
    ['omits language when the class is missing', '<pre><code>raw\ntext</code></pre>', '```\nraw\ntext\n```'],
    // A three-backtick fence would be closed early by the content's own
    // ```; the emitted fence must be longer.
    [
      'escalates the block fence past line-leading ``` runs in the content',
      '<pre><code class="language-markdown">```js\nlet x;\n```</code></pre>',
      '````markdown\n```js\nlet x;\n```\n````',
    ],
    [
      'keeps a three-backtick fence when ``` only appears mid-line',
      '<pre><code>use ``` to fence code</code></pre>',
      '```\nuse ``` to fence code\n```',
    ],
    // markdownEnhance injects [data-code-copy-mount] inside <pre>.
    // The serializer must skip that chrome.
    [
      'strips per-pre CopyButton mounts from output',
      '<pre><code class="language-go">fmt.Println("hi")</code><div data-code-copy-mount="true">copy</div></pre>',
      '```go\nfmt.Println("hi")\n```',
    ],
    // Shiki replaces the inner <code> with <span class="line"><span style>...
    // tokens. textContent walks through them as if they were a flat
    // string, so the serializer doesn't need to know about Shiki.
    [
      'walks Shiki token spans via textContent',
      '<pre class="shiki"><code class="language-typescript">' +
        '<span class="line"><span style="color:#abc">const</span><span> x</span></span>' +
        '<span class="line"><span style="color:#abc"> = 1;</span></span>' +
      '</code></pre>',
      '```typescript\nconst x = 1;\n```',
    ],
  ])('%s', (_name, html, expected) => {
    expect(serializeRangeToMarkdown(selectAll(asMarkdownBody(html)))).toBe(expected);
  });
});

describe('serializeRangeToMarkdown — math and mermaid', () => {
  it.each<[string, string, string]>([
    [
      'uses data-math-source for inline math (KaTeX wipes the original textContent)',
      '<p>see <span class="math-inline" data-math-source="x^2"><span class="katex">RENDERED</span></span></p>',
      'see $x^2$',
    ],
    [
      'uses data-math-source for display math',
      '<div class="math-display" data-math-source="\\sum_{i=1}^n i"><span class="katex">RENDERED</span></div>',
      '$$\n\\sum_{i=1}^n i\n$$',
    ],
    // Without rendered .katex evidence, the data attribute is not
    // trustworthy — copy must reflect what the user can see.
    [
      'ignores a forged data-math-source when KaTeX has not rendered into the node',
      '<p><span class="math-inline" data-math-source="$(rm -rf ~)">harmless</span></p>',
      '$harmless$',
    ],
    [
      'reads mermaid source from the current rendered host without copying hidden or SVG text',
      '<p>before</p>'
      + '<div class="mermaid streamdown-mermaid-host mermaid-host-with-fallback mermaid-rendered" '
      + 'data-mermaid-source="graph TD&#10;A to B">'
      + '<pre class="mermaid-source-fallback" aria-hidden="true">graph TD\nA to B</pre>'
      + '<div data-streamdown-mermaid="diagram-1"><div>'
      + '<svg data-mermaid-svg><svg><text>Rendered A</text><text>Rendered B</text></svg></svg>'
      + '</div></div></div>'
      + '<p>after</p>',
      'before\n\n```mermaid\ngraph TD\nA to B\n```\n\nafter',
    ],
    // The outer host SVG exists before Mermaid finishes. Only a nested
    // rendered SVG proves that the source attribute describes visible output.
    [
      'does not trust a data-mermaid-source host without a rendered nested SVG',
      '<div class="mermaid streamdown-mermaid-host" data-mermaid-source="evil source">'
      + '<pre><code>visible source</code></pre>'
      + '<svg data-mermaid-svg></svg>'
      + '</div>',
      '```\nvisible source\n```',
    ],
    [
      'keeps a current Mermaid host on block boundaries beside inline fragments',
      '<span>before</span>'
      + '<div class="mermaid streamdown-mermaid-host mermaid-rendered" '
      + 'data-mermaid-source="graph LR">'
      + '<svg data-mermaid-svg><svg><text>Rendered label</text></svg></svg>'
      + '</div>'
      + '<span>after</span>',
      'before\n```mermaid\ngraph LR\n```\n\nafter',
    ],
  ])('%s', (_name, html, expected) => {
    expect(serializeRangeToMarkdown(selectAll(asMarkdownBody(html)))).toBe(expected);
  });
});

describe('serializeRangeToMarkdown — tables', () => {
  it.each<[string, string, string]>([
    [
      'emits a GFM table with header separator',
      '<table><thead><tr><th>name</th><th>age</th></tr></thead>' +
      '<tbody><tr><td>Ada</td><td>36</td></tr><tr><td>Lin</td><td>27</td></tr></tbody></table>',
      [
        '| name | age |',
        '| --- | --- |',
        '| Ada | 36 |',
        '| Lin | 27 |',
      ].join('\n'),
    ],
    [
      'respects column alignment from inline style',
      '<table><thead><tr><th style="text-align:left">a</th>' +
      '<th style="text-align:center">b</th>' +
      '<th style="text-align:right">c</th></tr></thead>' +
      '<tbody><tr><td>1</td><td>2</td><td>3</td></tr></tbody></table>',
      [
        '| a | b | c |',
        '| :--- | :---: | ---: |',
        '| 1 | 2 | 3 |',
      ].join('\n'),
    ],
    [
      'escapes pipes inside cell content',
      '<table><thead><tr><th>op</th></tr></thead><tbody><tr><td>a | b</td></tr></tbody></table>',
      '| op |\n| --- |\n| a \\| b |',
    ],
    // Marked usually emits a thead; a hand-rolled HTML table might
    // not. Don't drop the body when the header is missing.
    [
      'emits body rows for a table that has no <thead>',
      '<table><tbody><tr><td>a</td><td>b</td></tr><tr><td>c</td><td>d</td></tr></tbody></table>',
      '| a | b |\n| c | d |',
    ],
  ])('%s', (_name, html, expected) => {
    expect(serializeRangeToMarkdown(selectAll(asMarkdownBody(html)))).toBe(expected);
  });

  it('keeps a pipe that follows a backslash inside its cell', () => {
    const host = asMarkdownBody(
      '<table><thead><tr><th>op</th></tr></thead><tbody><tr><td>a\\|b</td></tr></tbody></table>',
    );
    const markdown = serializeRangeToMarkdown(selectAll(host));
    expect(markdown).toBe('| op |\n| --- |\n| a\\\\\\|b |');
    // Read back by the app's own grammar: still one cell, same characters.
    type Node = { type: string; text?: string; tokens?: Node[] };
    const find = (nodes: Node[], type: string): Node[] =>
      nodes.flatMap((node) => (node.type === type ? [node] : find(node.tokens ?? [], type)));
    const body = find(lex(markdown ?? '') as Node[], 'tbody');
    const cells = find(body, 'td');
    expect(cells).toHaveLength(1);
    expect((cells[0]!.tokens ?? []).map((token) => token.text).join('')).toBe('a\\|b');
  });
});

describe('serializeRangeToMarkdown — nested blocks', () => {
  it.each<[string, string, string]>([
    [
      'preserves a code block nested inside a blockquote',
      '<blockquote><p>before</p><pre><code class="language-js">a()</code></pre></blockquote>',
      '> before\n>\n> ```js\n> a()\n> ```',
    ],
    [
      'preserves a blockquote nested inside a list item',
      '<ul><li><blockquote><p>note</p></blockquote></li></ul>',
      '- > note',
    ],
  ])('%s', (_name, html, expected) => {
    expect(serializeRangeToMarkdown(selectAll(asMarkdownBody(html)))).toBe(expected);
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
