// Markdown sources every find projection is held to: the markdownFindText
// browser tests render them and compare the runs, and the find candidate
// vectors (threadFindText.browser.test.ts, findCandidateVectors.json) hold the
// store's candidates to the runs they project.

export const HEX = '0123456789abcdef0123456789abcdef';
export const CORPUS = [
  'Plain words, and a sentence.',
  'Use **bold**, _em_, ~~gone~~ and `code_span()` here.',
  'A [link text](https://example.com/target) and <https://example.com/auto>.',
  '- one\n- two **2**\n  - nested\n- three\n\n1. first\n2. second',
  '- [ ] open task\n- [x] done task',
  '```ts\nconst needle = 1;\n```',
  '```suggestion\nreplacement line\n```',
  '> quoted **text**\n\nafter',
  '| Col A | Col B |\n| --- | --- |\n| a1 | `b1` |',
  '# Title\n\n## Sub title\n\nbody',
  '> [!NOTE]\n> An alert body.',
  'Escapes: \\*not em\\* and \\`tick\\`.',
  'Image ![alt words](https://example.com/x.png) after.',
  'line one  \nline two',
  '<details><summary>Summary words</summary>\n\nHidden body words\n\n</details>\n\nafter details',
  '<details open><summary>Open summary</summary>\n\nShown body words\n\n</details>',
  'Inline <b>bold html</b> and <code>tag code</code> and <span>stray span</span>.',
  '<table><tr><td>cell one</td><td>cell two</td></tr></table>',
  '<!-- a comment --> visible after comment',
  'Footnote ref[^1].\n\n[^1]: the note body',
  'Text with &amp; entity &lt;tag&gt; and &copy;.',
  '---\n\nafter rule',
  '```mermaid\ngraph TD; Start-->Finish\n```\n\nafter the diagram',
  '$$\nx^2 + yy\n$$\n\ninline $a+bb$ math words',
  `See ![shot alt](/uploads/${HEX}/shot.png) and [report.pdf](/uploads/${HEX}/report.pdf) here.`,
  `<img src="/uploads/${HEX}/embedded.png" alt="embedded alt"> after the embed`,
  '<table><tr><td><details><summary>cell sum</summary>inner cell words</details></td></tr></table>',
  // Every place the renderers drop, replace or reorder what the source says.
  '| Head | Other |\n| --- | --- |\n| top | x |\n| merged ^ | y |\n| below | z |',
  '| Head | Other |\n| --- | --- |\n| wide cell ||\n| a | b |',
  '[titled link](https://example.com "the title words") and [bad link](javascript:alert(1)) end',
  '[a file](src/a.ts), [an anchor](#frag) and [mail](mailto:x@example.com).',
  'Visit https://example.com/bare and www.example.org today.',
  'H~2~O and x^2^ power, ~5~10 approx',
  ': term words : detail words\n: second term : second detail',
  'Cites [1] [ref two] here.',
  'Open http://localhost:5173/app now.',
  '<Note kind="x">mdx child words</Note> after',
  'Copy &nbsp;non&#8209;breaking &#x41; and \\backslash',
  '[center]\naligned needle words\n[/center]\n\nafter the block',
  '[right]\nright needle\n\n- listed needle\n[/right]\n[center]\nnext needle\n[/center]',
  '[center]\nunclosed needle\n\n- listed needle\n\nlast needle',
  '[center]\n<Aside>\nFirst needle paragraph.\n\nSecond needle paragraph.\n</Aside>\n[/center]',
  '[center]\nneedle\n[/right]\n\nstill centered needle\n\n[/center]\n[/center]',
  '[typed](streamdown:incomplete-link) and [label][ref]\n\n[ref]: https://example.com/ref',
  '- item\n\n  loose paragraph\n\n- next',
  '| Name | Count |\n| --- | --- |\n| apples | 3 |\n| --- | --- |\n| total footer | 3 |',
  // A claimed video's fallback children, a <details> among them, are dropped.
  `<div><video src="/uploads/${HEX}/clip.mp4">needle<details><summary>fallback</summary>inner</details></video></div>\n\n<details open><summary>sum</summary>\n\nneedle BODY\n\n</details>\n\nneedle KEEP\n\nneedle NEXT`,
  // What skipped content or a dropped construct leaves joined in the source.
  'joined $m$ math and pic![alt](https://example.com/p.png)ture and [bad](javascript:alert(1))tail',
  '**foo**bar and [docs](https://example.com/x)suffix and AT&amp;T',
  // Link-like text the lexer shows as written beside markup it erases.
  'a \\[b](literal) c [d](https://example.com) e',
  'esc \\[x\\](q) and \\(p\\) then [real](https://example.com/r) end',
  'wiki [page](https://en.example.org/wiki/Foo_(bar)) after and x](z) tail',
  'nested [outer [inner](https://example.com/i) rest](https://example.com/o) after',
  'adjacent [one](https://example.com/1)[two](https://example.com/2) and ![pic](https://example.com/p.png)[three](https://example.com/3) end',
  'code `[x](q)` and `<b>` then [z](https://example.com/z) w',
  'entity &amp; [a&amp;c](https://example.com/e) &lt;c&gt; d',
  '[label]: https://example.com/l\n[text][label] and [a](https://example.com/a) c',
  '<https://example.com/a](q)> and [c](https://example.com/c) d',
  'see https://example.com/x](q) and [c](https://example.com/c) d',
  '[t](https://example.com "ti tle") and \\[u](v) w',
  'x <span title="a](q)">s</span> [z](https://example.com/z) w',
  'cost $a](q)$ and [c](https://example.com/c) d',
  'tag <composer@0.7s> label and [a](https://example.com/a) c',
  '~~gone [a](https://example.com/a)~~ and H~2~O [c](https://example.com/c) d',
  // Markup that hides what the store's candidate reader would read as code,
  // a tag or a fence (internal/store/find_markdown.go).
  'cite [x `q] z [a](https://example.com/a) `c end',
  'see http://`a [b](https://example.com/b) c` d `e [f](https://example.com/f) g',
  'gone ~~a `b~~ [c](https://example.com/c) d` e',
  'sub ~a<!--b~ c--> [d](https://example.com/d) e',
  'sup ^a<!--b^ c--> [d](https://example.com/d) e',
  'cost $$`x$$ [a](https://example.com/q) `c end',
  'a <!-- x\n[b](https://example.com/b) --> c [d](https://example.com/d) e',
  'x <Span>q</Span> [a](https://example.com/a) w',
  '> x <a b=\n> "1"> q [c](https://example.com/c) d',
  '| h | i |\n| --- | --- |\n| `a ``x | [b](https://example.com/q) w | z`` c` |',
  'a `b\nc [d](https://example.com/d) e` f [g](https://example.com/g) z',
  '> [!NOTE](https://example.com/n) x [a](https://example.com/a) w',
  '> [l]: https://example.com/l\n> [t][l] and [a](https://example.com/a) c',
  '- [l]: https://example.com/l\n  [t][l] and [a](https://example.com/a) c',
  '<div>\n```\n</div>\n\n[a](https://example.com/q) w\n\n```\nx\n```',
  '$$\n```\n$$\n\n[a](https://example.com/a) w\n\n```\nx\n```',
  '[^n]: note\n```\n\n[a](https://example.com/a) w\n\n```\nx\n```',
  '- a\n  ```\n  x\n\n[b](https://example.com/q) c\n\n```\nq\n```',
  '````markdown\n[a](https://example.com/q) w\n```\nx\n```\n````',
  // Embedded HTML a forge renders and review prose renders with it.
  'Inline <ins>added words</ins>, <mark>marked words</mark>, <abbr title="hover title">short form</abbr> and <a name="top">anchor words</a>.',
  'A <picture><source media="(prefers-color-scheme: dark)" srcset="https://example.com/d.png"><img src="https://example.com/l.png" alt="picture alt"></picture> picture.',
  '<picture>\n<source srcset="https://example.com/d.png">\n<img src="https://example.com/l.png" alt="block alt">\n</picture>\n\nafter picture',
  '<details><summary><h3>Heading summary</h3></summary>\n\nHeading body words\n\n</details>\n\nafter heading',
  '> [!NOTE]\n> Before words\n>\n> <details>\n> <summary>Quoted summary</summary>\n>\n> Quoted body words\n>\n> </details>\n>\n> After words',
  // An HTML wrapper around Markdown renders as one fragment (html regions).
  '<details>\n<summary>Analysis chain</summary><blockquote>\n\nChain **needle** words.\n\n<details>\n<summary>Nested chain</summary>\n\nnested chain words\n\n</details>\n\n</blockquote></details>\n\nafter chain',
  '<table>\n<tr><th>Head A</th><th>Head B</th></tr>\n<tr><td>\n\n**cell needle** words\n\n</td><td><code>cell code</code></td></tr>\n</table>\n\nafter table',
  '<div align="center">\n\n**centered needle** inside div\n\n</div>\n\nafter div',
  '<div>\n<details open><summary>authored summary</summary>\n\nregion body words\n\n<details><summary>island summary</summary>\n\nisland body words\n\n</details>\n\n</details>\nbetween words\n<details><summary>later summary</summary>later body words</details>\n\n- listed region words\n\n</div>',
  // A line break ends the run it sits in.
  'alpha\\\nbeta and gamma<br>delta then <p>x</p>',
  // Authored text that spells what a text marker for an island would.
  '<div>\n<p>&#1;0&#1;</p>\n\nneedle paragraph\n\n</div>',
  // GitLab's image attribute lists (shown as text off GitLab).
  `sized ![b](https://example.com/b.png){width=30 height=20px} after and ![u](/uploads/${HEX}/u.png){width=50%} end`,
  // The list after any image, however written; a space keeps it text.
  `listed <img src="https://example.com/i.png" alt="i">{width=20} and <img src="/uploads/${HEX}/v.png">{height=9} then ![s](https://example.com/s.png) {width=9} end`,
  // Emoji shortcodes, by each forge's names and rules.
  'Ship :rocket: and :tada::tada: then :shipit: done `:x:` a:b:c 10:30:00 \\:rocket\\: x:rocket:',
  '## Head :sparkles:',
  '| a |\n| --- |\n| b :x: |',
  // Forge references, mentions and the shapes that are not references.
  'Fixes !12+, group/widget#5 and group/other!7; see https://gitlab.com/group/widget/-/merge_requests/12/diffs, 0123456789abcdef0123456789abcdef01234567, @dev.one, a@b.example, [[Two Words]], [notes](docs/notes.md) and smb://host/share.',
  'Not references: \\#12 a#12 pkg!12 $5 %1 ~bug deadbeef 1234567 `#12`',
  // Themed and playable images, and an alt's plain text.
  'Logo <picture><source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/dark.png"><img alt="logo" src="https://example.invalid/default.png"></picture> end',
  'before ![x](https://example.invalid/a.png#gh-dark-mode-only) after',
  '<p>before <img src="https://example.invalid/x.png#gh-dark-mode-only"> after</p>',
  'see ![clip](https://example.invalid/clip.mp4) after',
  '![&copy; **alt**](https://example.invalid/i.png)',
  // Each forge's own syntax.
  'Struck ~one~ and ~~two~~',
  'Diff {+ added +} and {- removed -}',
  'Colors `#F00` and `\\#FF0000`',
  '>>>\nquoted one\n\nquoted two\n>>>',
  '- [~] skipped task\n- [ ]',
  '| Done | Task |\n| --- | --- |\n| [x] | cell task |',
  'Fruits\n: apple\n: orange',
  '```json:table\n{"items": [{"a": "11", "b": "**22**"}], "markdown": true, "caption": "Numbers"}\n```',
  '---\ntitle: About\n---\n\nbody',
  // What needs the whole document: definitions, footnotes, headings.
  'Use [ref] and [text][Ref].\n\n[ref]: https://example.invalid/ref "Title"\n\nAfter.',
  'One[^b] two[^a] [^missing].\n\n[^a]: Alpha note.\n[^b]: Beta note\nlazy line.\n[^unused]: Unused words.',
  '[[_TOC_]]\n\n# Heading one\n\n## Heading two\n\n[TOC]\n\nInline [toc] text.',
  '# Sample\n\n<a name="custom"></a><a id="ided"></a>\n\n[jump](#sample) and [custom](#custom)',
  // Block structure and HTML off the allowlist.
  '1. a\n1. b',
  '* a\n+ b',
  '> [!warning] Title\n> body',
  '$`c^2`$',
  '```math\nx\n```',
  'a <big>b</big> <script>x</script> c',
  'c <textarea>t</textarea> d',
  'Press <kbd><kbd>Ctrl</kbd>+<kbd>P</kbd></kbd>',
  '<a href="https://example.invalid/">\n\n## H\n\n</a>',
  '| a | b |\n| - | - |\n| x | ` |',
];

// Markdown pieces whose reading depends on what is around them, for the
// find candidate vectors: every pair, and seeded sequences of up to five
// in containers, each piece between words a query can span.
const PIECES = [
  '[x](https://e.io/u)', '\\[y](v)', '[z](w q)', '`[c](d)`', '![p](https://e.io/i.png)', '<b>', '</b>', '<!-- m -->',
  '<https://e.io/a>', 'https://e.io/b', '&amp;', '[r][s]', '](t)', '[', '`', '$m$', '~~d~~', '*e*', '~f~',
  '[n [o](https://e.io/o) p](https://e.io/n)', '[a](<https://e.io/l>)', '[t](https://e.io "ti")', '\n', '|',
];
const WRAPS: readonly ((inner: string) => string)[] = [
  (inner) => inner,
  (inner) => `- ${inner}`,
  (inner) => `> ${inner}`,
  (inner) => `| h |\n| --- |\n| ${inner.replace(/\n/g, ' ')} |`,
  (inner) => `# ${inner.replace(/\n/g, ' ')}`,
  (inner) => `\`\`\`\n${inner}\n\`\`\`\n\n${inner}`,
  (inner) => `[s]: https://e.io/s\n${inner}`,
];

export function findCandidateCombinations(): string[] {
  const join = (pieces: readonly string[]) => pieces.map((piece, at) => `k${at} ${piece}`).join(' ') + ` k${pieces.length}`;
  const sources: string[] = [];
  for (const first of PIECES) for (const second of PIECES) sources.push(join([first, second]));
  const next = random(7);
  for (let i = 0; i < 300; i++) {
    const pieces = Array.from({ length: 3 + Math.floor(next() * 3) }, () => PIECES[Math.floor(next() * PIECES.length)]);
    sources.push(WRAPS[Math.floor(next() * WRAPS.length)](join(pieces)));
  }
  return sources;
}

// Random comments of real markdown and HTML.
function random(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let t = state;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export function generatedMarkdown(seed: number): string {
  const next = random(seed);
  const pick = <T,>(items: readonly T[]): T => items[Math.floor(next() * items.length)];
  const words = () => Array.from({ length: 1 + Math.floor(next() * 4) }, () => pick(['needle', 'word', 'Needle', 'x', 'needleneedle'])).join(' ');
  const open = () => (next() < 0.5 ? ' open' : '');
  const block = (depth: number): string => {
    const kinds = depth > 2 ? 7 : 12;
    switch (Math.floor(next() * kinds)) {
      // Prose, with forge attachments as markdown: chips, neither counted nor painted.
      case 0: return next() < 0.5 ? words() : `${words()} [${words()}](/uploads/${HEX}/r.pdf) ![${words()}](/uploads/${HEX}/a.png) ${words()}`;
      case 1: return `> [!${pick(['NOTE', 'WARNING', 'TIP'])}]\n> ${words()}`;
      case 2: return `| ${words()} | b |\n| --- | --- |\n| ${words()} | \`${words()}\` |`;
      case 3: return '```\n' + words() + '\n```';
      case 4: return `<div><video src="/uploads/${HEX}/clip.mp4">${words()}<details><summary>${words()}</summary>${words()}</details></video></div>`;
      case 5: return `<div><script>${words()}()</script><foo>${words()}</foo><!-- ${words()} --> ${words()}</div>`;
      case 6: return `<div><video src="https://example.com/v.mp4">${words()}</video> <img src="/uploads/${HEX}/a.png" alt="${words()}"> ${words()}</div>`;
      case 7: return `<details${open()}><summary>${words()}</summary>\n\n${block(depth + 1)}\n\n</details>`;
      case 8: return `<details${open()}><summary>${words()}</summary>\n\n${block(depth + 1)}\n\n</details>\n<details${open()}><summary>${words()}</summary>\n\n${words()}\n\n</details>`;
      case 9: return `<table><tr><td><details${open()}><summary>${words()}</summary>${words()}</details></td><td>${words()}</td></tr></table>`;
      // Closed, unclosed (aligned to the end), or closed by the other kind (text).
      case 10: {
        const align = pick(['center', 'right']);
        const closer = pick([`\n[/${align}]`, '', `\n[/${align === 'center' ? 'right' : 'center'}]`]);
        return `[${align}]\n${words()}\n\n${block(depth + 1)}${closer}`;
      }
      default: return `- ${words()}\n- ${block(depth + 1).replace(/\n/g, ' ')}`;
    }
  };
  return Array.from({ length: 2 + Math.floor(next() * 4) }, () => block(0)).join('\n\n');
}

