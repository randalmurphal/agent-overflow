// Serializer contract for the clipboard's `text/html` flavor.
//
// Two things are being pinned here. The first is fidelity: headings,
// bold, tables, lists, task boxes and fenced code have to survive into
// a shape a rich paste target understands. The second, and the reason
// this suite is long, is that the tag allowlist is a STRUCTURAL
// property of the serializer — no sanitizer runs after it, so the only
// proof that hostile markdown can't emit a tag is that no branch would.
// The hostile battery below asserts that end-to-end, and
// `collectTags` re-derives the whole tag set from a document that tries
// every vector at once, so a future branch that adds an unallowlisted
// tag fails even if nobody remembers to write a case for it.

import { describe, expect, it } from 'vitest';
import { markdownToClipboardHtml } from './markdownHtmlSerialize';

/** Every tag the flavor is allowed to contain. */
const ALLOWED_TAGS = new Set([
  'p', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6',
  'ul', 'ol', 'li',
  'strong', 'em', 'del',
  'code', 'pre',
  'table', 'thead', 'tbody', 'tr', 'th', 'td',
  'blockquote', 'br', 'hr', 'a',
]);

function collectTags(html: string): Set<string> {
  const tags = new Set<string>();
  for (const match of html.matchAll(/<\/?([a-zA-Z][a-zA-Z0-9]*)/g)) {
    tags.add(match[1].toLowerCase());
  }
  return tags;
}

describe('markdownToClipboardHtml — inline', () => {
  it('emits strong / em / del', () => {
    expect(markdownToClipboardHtml('**b** *i* ~~s~~')).toBe(
      '<p><strong>b</strong> <em>i</em> <del>s</del></p>',
    );
  });

  it('emits inline code as <code>', () => {
    expect(markdownToClipboardHtml('use `npm i`')).toBe('<p>use <code>npm i</code></p>');
  });

  it('escapes & < > " in text', () => {
    expect(markdownToClipboardHtml('a & b < c > d " e')).toBe(
      '<p>a &amp; b &lt; c &gt; d &quot; e</p>',
    );
  });

  it('escapes inside code spans too', () => {
    expect(markdownToClipboardHtml('`a && b<c>`')).toBe(
      '<p><code>a &amp;&amp; b&lt;c&gt;</code></p>',
    );
  });

  it('emits an http(s) link as <a href>', () => {
    expect(markdownToClipboardHtml('[click](https://example.com/a(b))')).toBe(
      '<p><a href="https://example.com/a(b)">click</a></p>',
    );
  });

  it('emits a hard break as <br>', () => {
    expect(markdownToClipboardHtml('one  \ntwo')).toBe('<p>one<br>two</p>');
  });

  it('keeps an image as its alt text, never an <img>', () => {
    const html = markdownToClipboardHtml('![the alt](https://example.com/i.png)');
    expect(html).toBe('<p>the alt</p>');
    expect(html).not.toContain('<img');
  });
});

describe('markdownToClipboardHtml — blocks', () => {
  it('emits headings at their level, clamped to h1–h6', () => {
    expect(markdownToClipboardHtml('# one')).toBe('<h1>one</h1>');
    expect(markdownToClipboardHtml('###### six')).toBe('<h6>six</h6>');
  });

  it('emits paragraphs separately', () => {
    expect(markdownToClipboardHtml('one\n\ntwo')).toBe('<p>one</p><p>two</p>');
  });

  it('emits blockquotes with their block children intact', () => {
    expect(markdownToClipboardHtml('> quoted **b**')).toBe(
      '<blockquote><p>quoted <strong>b</strong></p></blockquote>',
    );
  });

  it('emits a GFM alert as a blockquote with a bold variant lead', () => {
    expect(markdownToClipboardHtml('> [!NOTE]\n> Careful.')).toBe(
      '<blockquote><p><strong>Note</strong></p><p>Careful.</p></blockquote>',
    );
  });

  it('emits a horizontal rule', () => {
    expect(markdownToClipboardHtml('---')).toBe('<hr>');
  });

  it('returns empty string for blank input', () => {
    expect(markdownToClipboardHtml('')).toBe('');
    expect(markdownToClipboardHtml('   \n\n  ')).toBe('');
  });
});

describe('markdownToClipboardHtml — code fences', () => {
  it('carries the fence language as a language- class', () => {
    expect(markdownToClipboardHtml('```ts\nconst x = 1;\n```')).toBe(
      '<pre><code class="language-ts">const x = 1;</code></pre>',
    );
  });

  it('emits an unlabelled fence without a class', () => {
    expect(markdownToClipboardHtml('```\nplain\n```')).toBe(
      '<pre><code>plain</code></pre>',
    );
  });

  it('escapes the code body', () => {
    expect(markdownToClipboardHtml('```html\n<div class="x">&</div>\n```')).toBe(
      '<pre><code class="language-html">&lt;div class=&quot;x&quot;&gt;&amp;&lt;/div&gt;</code></pre>',
    );
  });

  it('filters an info string that tries to close the class attribute', () => {
    const html = markdownToClipboardHtml('```js" onload="alert(1)\nx\n```');
    expect(html).toBe('<pre><code class="language-js">x</code></pre>');
    expect(html).not.toContain('onload');
  });

  it('emits a mermaid block as its source, not a rendered diagram', () => {
    expect(markdownToClipboardHtml('```mermaid\ngraph TD;\nA-->B;\n```')).toBe(
      '<pre><code class="language-mermaid">graph TD;\nA--&gt;B;</code></pre>',
    );
  });
});

describe('markdownToClipboardHtml — math', () => {
  it('emits display math source in a code block', () => {
    expect(markdownToClipboardHtml('$$\n\\frac{a}{b}\n$$')).toBe(
      '<pre><code class="language-math">\\frac{a}{b}</code></pre>',
    );
  });

  it('emits inline math source in a code span', () => {
    expect(markdownToClipboardHtml('value $x^2$ here')).toBe(
      '<p>value <code>x^2</code> here</p>',
    );
  });
});

describe('markdownToClipboardHtml — lists', () => {
  it('emits a tight unordered list', () => {
    expect(markdownToClipboardHtml('- a\n- b')).toBe('<ul><li>a</li><li>b</li></ul>');
  });

  it('emits a loose list item as a paragraph', () => {
    expect(markdownToClipboardHtml('- a\n\n- b')).toBe(
      '<ul><li><p>a</p></li><li><p>b</p></li></ul>',
    );
  });

  it('nests a sublist inside its parent item', () => {
    expect(markdownToClipboardHtml('1. one\n   - inner a\n   - inner b\n2. two')).toBe(
      '<ol><li>one<ul><li>inner a</li><li>inner b</li></ul></li><li>two</li></ol>',
    );
  });

  it('honors an ordered list start other than 1', () => {
    expect(markdownToClipboardHtml('5. five\n6. six')).toBe(
      '<ol start="5"><li>five</li><li>six</li></ol>',
    );
  });

  it('omits start when the list begins at 1', () => {
    expect(markdownToClipboardHtml('1. one')).toBe('<ol><li>one</li></ol>');
  });

  it('emits task-list state as a ballot character, not an <input>', () => {
    const html = markdownToClipboardHtml('- [x] done\n- [ ] todo');
    expect(html).toBe('<ul><li>☑ done</li><li>☐ todo</li></ul>');
    expect(html).not.toContain('<input');
  });

  it('puts the task mark inside a loose item paragraph', () => {
    expect(markdownToClipboardHtml('- [x] done\n\n- [ ] todo')).toBe(
      '<ul><li><p>☑ done</p></li><li><p>☐ todo</p></li></ul>',
    );
  });

  it('keeps inline markup alongside a task mark', () => {
    expect(markdownToClipboardHtml('- [x] ship **it**')).toBe(
      '<ul><li>☑ ship <strong>it</strong></li></ul>',
    );
  });
});

describe('markdownToClipboardHtml — tables', () => {
  it('emits thead / tbody with th and td cells', () => {
    expect(markdownToClipboardHtml('| A | B |\n| :-- | --: |\n| 1 | 2 |')).toBe(
      '<table><thead><tr><th>A</th><th>B</th></tr></thead>'
      + '<tbody><tr><td>1</td><td>2</td></tr></tbody></table>',
    );
  });

  it('keeps inline markup and escaping inside cells', () => {
    expect(markdownToClipboardHtml('| A |\n| --- |\n| **b** & `c` |')).toBe(
      '<table><thead><tr><th>A</th></tr></thead>'
      + '<tbody><tr><td><strong>b</strong> &amp; <code>c</code></td></tr></tbody></table>',
    );
  });

  it('keeps an empty cell so the row does not shift left', () => {
    expect(markdownToClipboardHtml('| A | B |\n| --- | --- |\n|  | 2 |')).toContain(
      '<tr><td></td><td>2</td></tr>',
    );
  });

  it('emits spans as integers and drops the rowspan continuation cell', () => {
    expect(markdownToClipboardHtml('| A | B |\n| --- | --- |\n| 1 || \n| ^ | 3 |')).toBe(
      '<table><thead><tr><th>A</th><th>B</th></tr></thead>'
      + '<tbody><tr><td colspan="2" rowspan="2">1</td></tr>'
      + '<tr><td>3</td></tr></tbody></table>',
    );
  });
});

describe('markdownToClipboardHtml — hostile input', () => {
  it('drops a raw <script> block, leaving no script tag and no payload', () => {
    const html = markdownToClipboardHtml('<script>alert(1)</script>\n\nafter');
    expect(html).toBe('<p>after</p>');
    expect(html).not.toContain('script');
    expect(html).not.toContain('alert');
  });

  it('drops raw <style> and <iframe> blocks', () => {
    const html = markdownToClipboardHtml(
      '<style>body{display:none}</style>\n\n<iframe src="https://evil.test"></iframe>\n\nkept',
    );
    expect(html).toBe('<p>kept</p>');
  });

  it('drops an inline raw HTML span carrying an event handler', () => {
    const html = markdownToClipboardHtml('hello <img src=x onerror=alert(1)> world');
    expect(html).toBe('<p>hello  world</p>');
    expect(html).not.toContain('onerror');
    expect(html).not.toContain('<img');
  });

  it('renders a javascript: link as plain text', () => {
    const html = markdownToClipboardHtml('[click](javascript:alert(1))');
    expect(html).toBe('<p>click</p>');
    expect(html).not.toContain('href');
  });

  it('renders a data: link as plain text', () => {
    expect(markdownToClipboardHtml('[x](data:text/html;base64,PHNjcmlwdD4=)')).toBe(
      '<p>x</p>',
    );
  });

  it('renders a vbscript: link as plain text', () => {
    expect(markdownToClipboardHtml('[x](vbscript:msgbox)')).toBe('<p>x</p>');
  });

  it('never produces an href for a scheme obfuscated with control characters', () => {
    // A C0 control inside the scheme is ignored by the paste target's
    // URL parser, so `java<0x01>script:` navigates — a naive
    // `startsWith('javascript:')` test would wave it through. Marked
    // refuses some of these destinations outright and resolves others
    // (angle-bracket and reference forms); all three must end up
    // without an anchor, whichever layer rejects them.
    const ctrl = String.fromCharCode(0x01);
    for (const source of [
      `[x](java${ctrl}script:alert(1))`,
      `[x](<java${ctrl}script:alert(1)>)`,
      `[x][d]\n\n[d]: java${ctrl}script:alert(1)`,
    ]) {
      const html = markdownToClipboardHtml(source);
      expect(html, source).not.toContain('href');
      expect(html, source).not.toContain('javascript:');
    }
  });

  it('renders a protocol-relative link as plain text', () => {
    expect(markdownToClipboardHtml('[x](//evil.test/p)')).toBe('<p>x</p>');
  });

  it('renders a schemeless relative link as plain text', () => {
    expect(markdownToClipboardHtml('[x](docs/guide.md)')).toBe('<p>x</p>');
  });

  it('escapes a quote smuggled into link text so the href cannot be reopened', () => {
    const html = markdownToClipboardHtml('[a" onclick="x](https://example.test/)');
    expect(html).toBe('<p><a href="https://example.test/">a&quot; onclick=&quot;x</a></p>');
  });

  it('escapes angle brackets that survive as text rather than tokenizing as HTML', () => {
    expect(markdownToClipboardHtml('5 < 6 > 4')).toBe('<p>5 &lt; 6 &gt; 4</p>');
  });

  it('emits only allowlisted tags for a document that tries every vector', () => {
    const hostile = [
      '# Head <script>a</script>',
      '',
      '<style>x{}</style>',
      '',
      '<iframe src="https://evil.test"></iframe>',
      '',
      'text with <b onclick="x">raw</b> and <img src=y onerror=z>',
      '',
      '[js](javascript:alert(1)) [data](data:text/html,x) [ok](https://ok.test/)',
      '',
      '- [x] task <script>b</script>',
      '- plain',
      '',
      '| H<script>c</script> | I |',
      '| --- | --- |',
      '| <iframe></iframe> | v |',
      '',
      '```js" onload="alert(1)',
      '<script>d</script>',
      '```',
      '',
      '> [!WARNING]',
      '> quoted <script>e</script>',
      '',
      '$$',
      '<script>f</script>',
      '$$',
      '',
      '![alt <script>g</script>](https://evil.test/i.png)',
    ].join('\n');

    const html = markdownToClipboardHtml(hostile);

    for (const tag of collectTags(html)) {
      expect(ALLOWED_TAGS, `unexpected tag <${tag}>`).toContain(tag);
    }
    expect(html).not.toContain('javascript:');
    expect(html).not.toContain('data:text/html');
    expect(html).not.toContain('onload');
    expect(html).not.toContain('onerror');
    expect(html).not.toContain('onclick');
    // Every href in the output is an absolute http(s) URL.
    for (const match of html.matchAll(/href="([^"]*)"/g)) {
      expect(match[1]).toMatch(/^https?:\/\//);
    }
  });
});
