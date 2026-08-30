/**
 * The grammar tables.
 *
 * Absorbed from marked 16.4.2 (`src/rules.ts`, MIT — see `../../LICENSE`).
 * Two classes of deliberate divergence live inline below, each marked with a
 * comment at the rule it changes:
 *
 * - **App overrides** (previously applied as `Lexer.rules.…` mutations from
 *   `parser/lexer.ts`): the `~~`-only strikethrough, the two mailto
 *   removals, and the homogeneous leading run in the GFM inline `text` rule.
 * - **Ported upstream fixes** from marked 17/18, cherry-picked by semantics:
 *   the 17.0.4 / 17.0.5 ReDoS fixes and the 18.0.4 / 18.0.6 / 18.0.7
 *   quadratic-backtracking fixes. Token-shape changes from those releases
 *   are deliberately NOT taken.
 *
 * The `other` table is likewise trimmed: the HTML-escape and URL-cleaning
 * entries served the Renderer half this tree never absorbed.
 */

const noopTest = { exec: () => null } as unknown as RegExp;

/**
 * PORTED from marked 18.0.4 (#3969): the list tokenizer builds five
 * indent-clamped regexes per list item, and the clamp has four distinct
 * values, so compiling them per call was pure waste on long lists.
 */
function cachedIndentRegex(createRegex: (indent: number) => RegExp) {
  const cache: RegExp[] = [];
  return (indent: number) => {
    const cacheIndex = Math.max(0, Math.min(3, indent - 1));
    let regex = cache[cacheIndex];
    if (!regex) {
      regex = createRegex(cacheIndex);
      cache[cacheIndex] = regex;
    }
    return regex;
  };
}

function edit(regex: string | RegExp, opt = '') {
  let source = typeof regex === 'string' ? regex : regex.source;
  const obj = {
    replace: (name: string | RegExp, val: string | RegExp) => {
      let valSource = typeof val === 'string' ? val : val.source;
      valSource = valSource.replace(other.caret, '$1');
      source = source.replace(name, valSource);
      return obj;
    },
    getRegex: () => {
      return new RegExp(source, opt);
    },
  };
  return obj;
}

const supportsLookbehind = (() => {
try {
  return !!new RegExp('(?<=1)(?<!1)');
} catch {
  // See browser support here:
  // https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Regular_expressions/Lookbehind_assertion
  return false;
}
})();

export const other = {
  codeRemoveIndent: /^(?: {1,4}| {0,3}\t)/gm,
  outputLinkReplace: /\\([\[\]])/g,
  indentCodeCompensation: /^(\s+)(?:```)/,
  beginningSpace: /^\s+/,
  endingHash: /#$/,
  startingSpaceChar: /^ /,
  endingSpaceChar: / $/,
  nonSpaceChar: /[^ ]/,
  newLineCharGlobal: /\n/g,
  tabCharGlobal: /\t/g,
  multipleSpaceGlobal: /\s+/g,
  blankLine: /^[ \t]*$/,
  doubleBlankLine: /\n[ \t]*\n[ \t]*$/,
  blockquoteStart: /^ {0,3}>/,
  blockquoteSetextReplace: /\n {0,3}((?:=+|-+) *)(?=\n|$)/g,
  blockquoteSetextReplace2: /^ {0,3}>[ \t]?/gm,
  listReplaceTabs: /^\t+/,
  listReplaceNesting: /^ {1,4}(?=( {4})*[^ ])/g,
  listIsTask: /^\[[ xX]\] /,
  listReplaceTask: /^\[[ xX]\] +/,
  anyLine: /\n.*\n/,
  hrefBrackets: /^<(.*)>$/,
  tableDelimiter: /[:|]/,
  tableAlignChars: /^\||\| *$/g,
  tableRowBlankLine: /\n[ \t]*$/,
  tableAlignRight: /^ *-+: *$/,
  tableAlignCenter: /^ *:-+: *$/,
  tableAlignLeft: /^ *:-+ *$/,
  startATag: /^<a /i,
  endATag: /^<\/a>/i,
  startPreScriptTag: /^<(pre|code|kbd|script)(\s|>)/i,
  endPreScriptTag: /^<\/(pre|code|kbd|script)(\s|>)/i,
  startAngleBracket: /^</,
  endAngleBracket: />$/,
  pedanticHrefTitle: /^([^'"]*[^\s])\s+(['"])(.*)\2/,
  unicodeAlphaNumeric: /[\p{L}\p{N}]/u,
  caret: /(^|[^\[])\^/g,
  findPipe: /\|/g,
  splitPipe: / \|/,
  slashPipe: /\\\|/g,
  carriageReturn: /\r\n|\r/g,
  spaceLine: /^ +$/gm,
  listItemRegex: (bull: string) => new RegExp(`^( {0,3}${bull})((?:[\t ][^\\n]*)?(?:\\n|$))`),
  nextBulletRegex: cachedIndentRegex((indent: number) => new RegExp(`^ {0,${indent}}(?:[*+-]|\\d{1,9}[.)])((?:[ \t][^\\n]*)?(?:\\n|$))`)),
  hrRegex: cachedIndentRegex((indent: number) => new RegExp(`^ {0,${indent}}((?:- *){3,}|(?:_ *){3,}|(?:\\* *){3,})(?:\\n+|$)`)),
  fencesBeginRegex: cachedIndentRegex((indent: number) => new RegExp(`^ {0,${indent}}(?:\`\`\`|~~~)`)),
  headingBeginRegex: cachedIndentRegex((indent: number) => new RegExp(`^ {0,${indent}}#`)),
  htmlBeginRegex: cachedIndentRegex((indent: number) => new RegExp(`^ {0,${indent}}<(?:[a-z].*>|!--)`, 'i')),
};

/**
 * Block-Level Grammar
 */

const newline = /^(?:[ \t]*(?:\n|$))+/;
const blockCode = /^((?: {4}| {0,3}\t)[^\n]+(?:\n(?:[ \t]*(?:\n|$))*)?)+/;
const fences = /^ {0,3}(`{3,}(?=[^`\n]*(?:\n|$))|~{3,})([^\n]*)(?:\n|$)(?:|([\s\S]*?)(?:\n|$))(?: {0,3}\1[~`]* *(?=\n|$)|$)/;
const hr = /^ {0,3}((?:-[\t ]*){3,}|(?:_[ \t]*){3,}|(?:\*[\t ]*){3,})(?:\n+|$)/;
const heading = /^ {0,3}(#{1,6})(?=\s|$)(.*)(?:\n+|$)/;
const bullet = /(?:[*+-]|\d{1,9}[.)])/;
const lheadingCore = /^(?!bull |blockCode|fences|blockquote|heading|html|table)((?:.|\n(?!\s*?\n|bull |blockCode|fences|blockquote|heading|html|table))+?)\n {0,3}(=+|-+) *(?:\n+|$)/;
const lheading = edit(lheadingCore)
  .replace(/bull/g, bullet) // lists can interrupt
  .replace(/blockCode/g, /(?: {4}| {0,3}\t)/) // indented code blocks can interrupt
  .replace(/fences/g, / {0,3}(?:`{3,}|~{3,})/) // fenced code blocks can interrupt
  .replace(/blockquote/g, / {0,3}>/) // blockquote can interrupt
  .replace(/heading/g, / {0,3}#{1,6}/) // ATX heading can interrupt
  .replace(/html/g, / {0,3}<[^\n>]+>\n/) // block html can interrupt
  .replace(/\|table/g, '') // table not in commonmark
  .getRegex();
const lheadingGfm = edit(lheadingCore)
  .replace(/bull/g, bullet) // lists can interrupt
  .replace(/blockCode/g, /(?: {4}| {0,3}\t)/) // indented code blocks can interrupt
  .replace(/fences/g, / {0,3}(?:`{3,}|~{3,})/) // fenced code blocks can interrupt
  .replace(/blockquote/g, / {0,3}>/) // blockquote can interrupt
  .replace(/heading/g, / {0,3}#{1,6}/) // ATX heading can interrupt
  .replace(/html/g, / {0,3}<[^\n>]+>\n/) // block html can interrupt
  .replace(/table/g, / {0,3}\|?(?:[:\- ]*\|)+[\:\- ]*\n/) // table can interrupt
  .getRegex();
const _paragraph = /^([^\n]+(?:\n(?!hr|heading|lheading|blockquote|fences|list|html|table| +\n)[^\n]+)*)/;
const blockText = /^[^\n]+/;
const _blockLabel = /(?!\s*\])(?:\\[\s\S]|[^\[\]\\])+/;
const def = edit(/^ {0,3}\[(label)\]: *(?:\n[ \t]*)?([^<\s][^\s]*|<.*?>)(?:(?: +(?:\n[ \t]*)?| *\n[ \t]*)(title))? *(?:\n+|$)/)
  .replace('label', _blockLabel)
  .replace('title', /(?:"(?:\\"?|[^"\\])*"|'[^'\n]*(?:\n[^'\n]+)*\n?'|\([^()]*\))/)
  .getRegex();

const list = edit(/^( {0,3}bull)([ \t][^\n]+?)?(?:\n|$)/)
  .replace(/bull/g, bullet)
  .getRegex();

const _tag = 'address|article|aside|base|basefont|blockquote|body|caption'
  + '|center|col|colgroup|dd|details|dialog|dir|div|dl|dt|fieldset|figcaption'
  + '|figure|footer|form|frame|frameset|h[1-6]|head|header|hr|html|iframe'
  + '|legend|li|link|main|menu|menuitem|meta|nav|noframes|ol|optgroup|option'
  + '|p|param|search|section|summary|table|tbody|td|tfoot|th|thead|title'
  + '|tr|track|ul';
const _comment = /<!--(?:-?>|[\s\S]*?(?:-->|$))/;
// PORTED from marked 18.0.7 (#4014): branch (1)'s close ended in `\n+`, so at
// EOF the close could not match and the engine retried every split of the
// lazy `[\s\S]*?` before falling through to `$` — quadratic. `\n*` closes on
// the first match and consumes identical text whenever a newline is present.
// (Branches 3-5 already read `\n*` at 16.4.2; upstream's edit to them was
// undoing its own 18.0.6 change, which this base never had.)
const html = edit(
  '^ {0,3}(?:' // optional indentation
+ '<(script|pre|style|textarea)[\\s>][\\s\\S]*?(?:</\\1>[^\\n]*\\n*|$)' // (1)
+ '|comment[^\\n]*(\\n+|$)' // (2)
+ '|<\\?[\\s\\S]*?(?:\\?>\\n*|$)' // (3)
+ '|<![A-Z][\\s\\S]*?(?:>\\n*|$)' // (4)
+ '|<!\\[CDATA\\[[\\s\\S]*?(?:\\]\\]>\\n*|$)' // (5)
+ '|</?(tag)(?: +|\\n|/?>)[\\s\\S]*?(?:(?:\\n[ \t]*)+\\n|$)' // (6)
+ '|<(?!script|pre|style|textarea)([a-z][\\w-]*)(?:attribute)*? */?>(?=[ \\t]*(?:\\n|$))[\\s\\S]*?(?:(?:\\n[ \t]*)+\\n|$)' // (7) open tag
+ '|</(?!script|pre|style|textarea)[a-z][\\w-]*\\s*>(?=[ \\t]*(?:\\n|$))[\\s\\S]*?(?:(?:\\n[ \t]*)+\\n|$)' // (7) closing tag
+ ')', 'i')
  .replace('comment', _comment)
  .replace('tag', _tag)
  .replace('attribute', / +[a-zA-Z:_][\w.:-]*(?: *= *"[^"\n]*"| *= *'[^'\n]*'| *= *[^\s"'=<>`]+)?/)
  .getRegex();

// PORTED from marked 18.0.7 (#4014), at all three `fences` interrupt sites
// below: `~{3,}` is unguarded and overlaps the `[^\n]*` that follows it, so a
// long newline-less tilde run backtracks quadratically. `~~~` matches the same
// strings without the overlap. The real `fences` tokenizer is untouched.
const paragraph = edit(_paragraph)
  .replace('hr', hr)
  .replace('heading', ' {0,3}#{1,6}(?:\\s|$)')
  .replace('|lheading', '') // setext headings don't interrupt commonmark paragraphs
  .replace('|table', '')
  .replace('blockquote', ' {0,3}>')
  .replace('fences', ' {0,3}(?:`{3,}(?=[^`\\n]*\\n)|~~~)[^\\n]*\\n')
  .replace('list', ' {0,3}(?:[*+-]|1[.)]) ') // only lists starting from 1 can interrupt
  .replace('html', '</?(?:tag)(?: +|\\n|/?>)|<(?:script|pre|style|textarea|!--)')
  .replace('tag', _tag) // pars can be interrupted by type (6) html blocks
  .getRegex();

const blockquote = edit(/^( {0,3}> ?(paragraph|[^\n]*)(?:\n|$))+/)
  .replace('paragraph', paragraph)
  .getRegex();

/**
 * Normal Block Grammar
 */

const blockNormal = {
  blockquote,
  code: blockCode,
  def,
  fences,
  heading,
  hr,
  html,
  lheading,
  list,
  newline,
  paragraph,
  table: noopTest,
  text: blockText,
};

type BlockKeys = keyof typeof blockNormal;

/**
 * GFM Block Grammar
 */

const gfmTable = edit(
  '^ *([^\\n ].*)\\n' // Header
+ ' {0,3}((?:\\| *)?:?-+:? *(?:\\| *:?-+:? *)*(?:\\| *)?)' // Align
+ '(?:\\n((?:(?! *\\n|hr|heading|blockquote|code|fences|list|html).*(?:\\n|$))*)\\n*|$)') // Cells
  .replace('hr', hr)
  .replace('heading', ' {0,3}#{1,6}(?:\\s|$)')
  .replace('blockquote', ' {0,3}>')
  .replace('code', '(?: {4}| {0,3}\t)[^\\n]')
  .replace('fences', ' {0,3}(?:`{3,}(?=[^`\\n]*\\n)|~~~)[^\\n]*\\n')
  .replace('list', ' {0,3}(?:[*+-]|1[.)]) ') // only lists starting from 1 can interrupt
  .replace('html', '</?(?:tag)(?: +|\\n|/?>)|<(?:script|pre|style|textarea|!--)')
  .replace('tag', _tag) // tables can be interrupted by type (6) html blocks
  .getRegex();

const blockGfm: Record<BlockKeys, RegExp> = {
  ...blockNormal,
  lheading: lheadingGfm,
  table: gfmTable,
  paragraph: edit(_paragraph)
    .replace('hr', hr)
    .replace('heading', ' {0,3}#{1,6}(?:\\s|$)')
    .replace('|lheading', '') // setext headings don't interrupt commonmark paragraphs
    .replace('table', gfmTable) // interrupt paragraphs with table
    .replace('blockquote', ' {0,3}>')
    .replace('fences', ' {0,3}(?:`{3,}(?=[^`\\n]*\\n)|~~~)[^\\n]*\\n')
    .replace('list', ' {0,3}(?:[*+-]|1[.)]) ') // only lists starting from 1 can interrupt
    .replace('html', '</?(?:tag)(?: +|\\n|/?>)|<(?:script|pre|style|textarea|!--)')
    .replace('tag', _tag) // pars can be interrupted by type (6) html blocks
    .getRegex(),
};

/**
 * Pedantic grammar (original John Gruber's loose markdown specification)
 */

const blockPedantic: Record<BlockKeys, RegExp> = {
  ...blockNormal,
  html: edit(
    '^ *(?:comment *(?:\\n|\\s*$)'
    + '|<(tag)[\\s\\S]+?</\\1> *(?:\\n{2,}|\\s*$)' // closed tag
    + '|<tag(?:"[^"]*"|\'[^\']*\'|\\s[^\'"/>\\s]*)*?/?> *(?:\\n{2,}|\\s*$))')
    .replace('comment', _comment)
    .replace(/tag/g, '(?!(?:'
      + 'a|em|strong|small|s|cite|q|dfn|abbr|data|time|code|var|samp|kbd|sub'
      + '|sup|i|b|u|mark|ruby|rt|rp|bdi|bdo|span|br|wbr|ins|del|img)'
      + '\\b)\\w+(?!:|[^\\w\\s@]*@)\\b')
    .getRegex(),
  def: /^ *\[([^\]]+)\]: *<?([^\s>]+)>?(?: +(["(][^\n]+[")]))? *(?:\n+|$)/,
  heading: /^(#{1,6})(.*)(?:\n+|$)/,
  fences: noopTest, // fences not supported
  lheading: /^(.+?)\n {0,3}(=+|-+) *(?:\n+|$)/,
  paragraph: edit(_paragraph)
    .replace('hr', hr)
    .replace('heading', ' *#{1,6} *[^\n]')
    .replace('lheading', lheading)
    .replace('|table', '')
    .replace('blockquote', ' {0,3}>')
    .replace('|fences', '')
    .replace('|list', '')
    .replace('|html', '')
    .replace('|tag', '')
    .getRegex(),
};

/**
 * Inline-Level Grammar
 */

const escape = /^\\([!"#$%&'()*+,\-./:;<=>?@\[\]\\^_`{|}~])/;
const inlineCode = /^(`+)([^`]|[^`][\s\S]*?[^`])\1(?!`)/;
const br = /^( {2,}|\\)\n(?!\s*$)/;
const inlineText = /^(`+|[^`])(?:(?= {2,}\n)|[\s\S]*?(?:(?=[\\<!\[`*_]|\b_|$)|[^ ](?= {2,}\n)))/;

// list of unicode punctuation marks, plus any missing characters from CommonMark spec
const _punctuation = /[\p{P}\p{S}]/u;
const _punctuationOrSpace = /[\s\p{P}\p{S}]/u;
const _notPunctuationOrSpace = /[^\s\p{P}\p{S}]/u;
const punctuation = edit(/^((?![*_])punctSpace)/, 'u')
  .replace(/punctSpace/g, _punctuationOrSpace).getRegex();

// GFM allows ~ inside strong and em for strikethrough
const _punctuationGfmStrongEm = /(?!~)[\p{P}\p{S}]/u;
const _punctuationOrSpaceGfmStrongEm = /(?!~)[\s\p{P}\p{S}]/u;
const _notPunctuationOrSpaceGfmStrongEm = /(?:[^\s\p{P}\p{S}]|~)/u;

// sequences em should skip over [title](link), `code`, <html>
const blockSkip = edit(/link|precode-code|html/, 'g')
  .replace('link', /\[(?:[^\[\]`]|(?<a>`+)[^`]+\k<a>(?!`))*?\]\((?:\\[\s\S]|[^\\\(\)]|\((?:\\[\s\S]|[^\\\(\)])*\))*\)/)
  .replace('precode-', supportsLookbehind ? '(?<!`)()' : '(^^|[^`])')
  .replace('code', /(?<b>`+)[^`]+\k<b>(?!`)/)
  .replace('html', /<(?! )[^<>]*?>/)
  .getRegex();

const emStrongLDelimCore = /^(?:\*+(?:((?!\*)punct)|[^\s*]))|^_+(?:((?!_)punct)|([^\s_]))/;

const emStrongLDelim = edit(emStrongLDelimCore, 'u')
  .replace(/punct/g, _punctuation)
  .getRegex();

const emStrongLDelimGfm = edit(emStrongLDelimCore, 'u')
  .replace(/punct/g, _punctuationGfmStrongEm)
  .getRegex();

const emStrongRDelimAstCore =
  '^[^_*]*?__[^_*]*?\\*[^_*]*?(?=__)' // Skip orphan inside strong
+ '|[^*]+(?=[^*])' // Consume to delim
+ '|(?!\\*)punct(\\*+)(?=[\\s]|$)' // (1) #*** can only be a Right Delimiter
+ '|notPunctSpace(\\*+)(?!\\*)(?=punctSpace|$)' // (2) a***#, a*** can only be a Right Delimiter
+ '|(?!\\*)punctSpace(\\*+)(?=notPunctSpace)' // (3) #***a, ***a can only be Left Delimiter
+ '|[\\s](\\*+)(?!\\*)(?=punct)' // (4) ***# can only be Left Delimiter
+ '|(?!\\*)punct(\\*+)(?!\\*)(?=punct)' // (5) #***# can be either Left or Right Delimiter
+ '|notPunctSpace(\\*+)(?=notPunctSpace)'; // (6) a***a can be either Left or Right Delimiter

const emStrongRDelimAst = edit(emStrongRDelimAstCore, 'gu')
  .replace(/notPunctSpace/g, _notPunctuationOrSpace)
  .replace(/punctSpace/g, _punctuationOrSpace)
  .replace(/punct/g, _punctuation)
  .getRegex();

const emStrongRDelimAstGfm = edit(emStrongRDelimAstCore, 'gu')
  .replace(/notPunctSpace/g, _notPunctuationOrSpaceGfmStrongEm)
  .replace(/punctSpace/g, _punctuationOrSpaceGfmStrongEm)
  .replace(/punct/g, _punctuationGfmStrongEm)
  .getRegex();

// (6) Not allowed for _
const emStrongRDelimUnd = edit(
  '^[^_*]*?\\*\\*[^_*]*?_[^_*]*?(?=\\*\\*)' // Skip orphan inside strong
+ '|[^_]+(?=[^_])' // Consume to delim
+ '|(?!_)punct(_+)(?=[\\s]|$)' // (1) #___ can only be a Right Delimiter
+ '|notPunctSpace(_+)(?!_)(?=punctSpace|$)' // (2) a___#, a___ can only be a Right Delimiter
+ '|(?!_)punctSpace(_+)(?=notPunctSpace)' // (3) #___a, ___a can only be Left Delimiter
+ '|[\\s](_+)(?!_)(?=punct)' // (4) ___# can only be Left Delimiter
+ '|(?!_)punct(_+)(?!_)(?=punct)', 'gu') // (5) #___# can be either Left or Right Delimiter
  .replace(/notPunctSpace/g, _notPunctuationOrSpace)
  .replace(/punctSpace/g, _punctuationOrSpace)
  .replace(/punct/g, _punctuation)
  .getRegex();

const anyPunctuation = edit(/\\(punct)/, 'gu')
  .replace(/punct/g, _punctuation)
  .getRegex();

// APP OVERRIDE: marked's autolink alternative for bare email addresses is
// gone. Agent prose commonly writes labels like `<composer@0.7s>`; marked saw
// an email address, the renderer rejected the resulting `mailto:` URL, and a
// visible `[blocked]` marker landed mid-sentence. Scheme autolinks are
// untouched. Applies to every grammar, so it is baked into the base rule.
const autolink = edit(/^<(scheme:[^\s\x00-\x1f<>]*)>/)
  .replace('scheme', /[a-zA-Z][a-zA-Z0-9+.-]{1,31}/)
  .getRegex();

const _inlineComment = edit(_comment).replace('(?:-->|$)', '-->').getRegex();
const tag = edit(
  '^comment'
    + '|^</[a-zA-Z][\\w:-]*\\s*>' // self-closing tag
    + '|^<[a-zA-Z][\\w-]*(?:attribute)*?\\s*/?>' // open tag
    + '|^<\\?[\\s\\S]*?\\?>' // processing instruction, e.g. <?php ?>
    + '|^<![a-zA-Z]+\\s[\\s\\S]*?>' // declaration, e.g. <!DOCTYPE html>
    + '|^<!\\[CDATA\\[[\\s\\S]*?\\]\\]>') // CDATA section
  .replace('comment', _inlineComment)
  .replace('attribute', /\s+[a-zA-Z:_][\w.:-]*(?:\s*=\s*"[^"]*"|\s*=\s*'[^']*'|\s*=\s*[^\s"'=<>`]+)?/)
  .getRegex();

// PORTED from marked 17.0.5 (#3918): catastrophic backtracking on an unclosed
// link label containing backticks. The backtick alternative now refuses to
// start mid-run, and a trailing run directly before `]` gets its own branch.
const _inlineLabel = /(?:\[(?:\\[\s\S]|[^\[\]\\])*\]|\\[\s\S]|`+(?!`)[^`]*?`+(?!`)|``+(?=\])|[^\[\]\\`])*?/;

// PORTED from marked 17.0.4 (#3902), the title separator: `[ \t]*` let the
// title group be probed at every backtrack position of the greedy href group,
// which is O(n^2) per call and O(n^3) across the inline pass. Requiring real
// whitespace matches CommonMark and kills the cascade.
// PORTED from marked 18.0.6 (#4013), the href group: `[^ \t\n\x00-\x1f]*` can
// match empty at every position, so a `[text](` with no nearby `)` backtracks
// quadratically. `+` plus an explicit empty-before-`)` branch is the same
// language without the ambiguity.
const link = edit(/^!?\[(label)\]\(\s*(href)(?:(?:[ \t]+(?:\n[ \t]*)?|\n[ \t]*)(title))?\s*\)/)
  .replace('label', _inlineLabel)
  .replace('href', /<(?:\\.|[^\n<>\\])+>|[^ \t\n\x00-\x1f]+|(?=\))/)
  .replace('title', /"(?:\\"?|[^"\\])*"|'(?:\\'?|[^'\\])*'|\((?:\\\)?|[^)\\])*\)/)
  .getRegex();

const reflink = edit(/^!?\[(label)\]\[(ref)\]/)
  .replace('label', _inlineLabel)
  .replace('ref', _blockLabel)
  .getRegex();

const nolink = edit(/^!?\[(ref)\](?:\[\])?/)
  .replace('ref', _blockLabel)
  .getRegex();

const reflinkSearch = edit('reflink|nolink(?!\\()', 'g')
  .replace('reflink', reflink)
  .replace('nolink', nolink)
  .getRegex();

const _caseInsensitiveProtocol = /[hH][tT][tT][pP][sS]?|[fF][tT][pP]/;

/**
 * Normal Inline Grammar
 */

const inlineNormal = {
  _backpedal: noopTest, // only used for GFM url
  anyPunctuation,
  autolink,
  blockSkip,
  br,
  code: inlineCode,
  del: noopTest,
  emStrongLDelim,
  emStrongRDelimAst,
  emStrongRDelimUnd,
  escape,
  link,
  nolink,
  punctuation,
  reflink,
  reflinkSearch,
  tag,
  text: inlineText,
  url: noopTest,
};

type InlineKeys = keyof typeof inlineNormal;

/**
 * Pedantic Inline Grammar
 */

const inlinePedantic: Record<InlineKeys, RegExp> = {
  ...inlineNormal,
  link: edit(/^!?\[(label)\]\((.*?)\)/)
    .replace('label', _inlineLabel)
    .getRegex(),
  reflink: edit(/^!?\[(label)\]\s*\[([^\]]*)\]/)
    .replace('label', _inlineLabel)
    .getRegex(),
};

/**
 * GFM Inline Grammar
 */

const inlineGfm: Record<InlineKeys, RegExp> = {
  ...inlineNormal,
  emStrongRDelimAst: emStrongRDelimAstGfm,
  emStrongLDelim: emStrongLDelimGfm,
  // APP OVERRIDE: the bare-email alternative is dropped here too — see
  // `autolink` above. Only the scheme/`www.` forms remain, which is why
  // `Tokenizer.url` can never see an `@` capture.
  url: edit(/^((?:protocol):\/\/|www\.)(?:[a-zA-Z0-9\-]+\.?)+[^\s<]*/)
    .replace('protocol', _caseInsensitiveProtocol)
    .getRegex(),
  _backpedal: /(?:[^?!.,:;*_'"~()&]+|\([^)]*\)|&(?![a-zA-Z0-9]+;$)|[?!.,:;*_'"~)]+(?!$))+/,
  // APP OVERRIDE: `(~~?)` -> `(~~)`. marked reads a single `~text~` as
  // strikethrough, so approximate values in agent prose (`~240MB`, `~5s`)
  // rendered struck through whenever two appeared on one line.
  del: /^(~~)(?=[^\s~])((?:\\[\s\S]|[^\\])*?(?:\\[\s\S]|[^\s~\\]))\1(?=[^~]|$)/,
  // APP OVERRIDE: the leading class was `[`~]+`, a COMBINED run of backticks
  // AND tildes, so a `~` fused to a backtick (`~`code``) swallowed the
  // backtick into a literal text run and the code span never opened (or
  // mis-paired onto a later backtick, leaving a stray pill mid-line). Split
  // into homogeneous runs so a backtick is never consumed as part of a tilde
  // run. Mirrors the `del` override above.
  text: edit(/^(`+|~+|[^`~])(?:(?= {2,}\n)|(?=[a-zA-Z0-9.!#$%&'*+\/=?_`{\|}~-]+@)|[\s\S]*?(?:(?=[\\<!\[`*~_]|\b_|protocol:\/\/|www\.|$)|[^ ](?= {2,}\n)|[^a-zA-Z0-9.!#$%&'*+\/=?_`{\|}~-](?=[a-zA-Z0-9.!#$%&'*+\/=?_`{\|}~-]+@)))/)
    .replace('protocol', _caseInsensitiveProtocol)
    .getRegex(),
};

/**
 * GFM + Line Breaks Inline Grammar
 */

const inlineBreaks: Record<InlineKeys, RegExp> = {
  ...inlineGfm,
  br: edit(br).replace('{2,}', '*').getRegex(),
  text: edit(inlineGfm.text)
    .replace('\\b_', '\\b_| {2,}\\n')
    .replace(/\{2,\}/g, '*')
    .getRegex(),
};

/**
 * exports
 */

export const block = {
  normal: blockNormal,
  gfm: blockGfm,
  pedantic: blockPedantic,
};

export const inline = {
  normal: inlineNormal,
  gfm: inlineGfm,
  breaks: inlineBreaks,
  pedantic: inlinePedantic,
};

export interface Rules {
  other: typeof other;
  block: Record<BlockKeys, RegExp>;
  inline: Record<InlineKeys, RegExp>;
}
