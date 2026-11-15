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
];

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

