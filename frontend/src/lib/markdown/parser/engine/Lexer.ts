/**
 * The block/inline lexer.
 *
 * Absorbed from marked 16.4.2 (`src/Lexer.ts`, MIT — see `../../LICENSE`),
 * with four classes of deliberate divergence, each commented at its site:
 *
 * - **Allocation-free extension dispatch** (previously
 *   `patches/marked@16.4.2.patch`): one typed receiver per Lexer and indexed
 *   loops, replacing a closure plus a fresh `{ lexer: this }` receiver per
 *   extension candidate at every token position. This tree registers ~20
 *   extensions and re-lexes the volatile tail on every reveal tick, so those
 *   were the hottest allocations in the parser.
 * - **Ported upstream fixes** from marked 18.0.7 / 18.0.10 / 18.0.11: the
 *   quadratic masked-source rebuild, the mask-length bug the rewrite exposed,
 *   and the per-call reflink-mask preamble.
 * - **Honest class surface** for the two pieces of per-document state this
 *   tree hangs on a Lexer: `inlineQueue` (marked declared it private; the
 *   list extension has to rewind it) and `footnotes`.
 * - **Linear block and inline runs**: the runs in progress are published
 *   (`blockRun`, `inlineRun`) so an extension can scan a run once, and
 *   paragraph clipping runs only where a paragraph is built.
 *
 * marked's `Parser`, `Renderer`, `Hooks`, `Instance` and the `marked()`
 * façade are NOT absorbed: this tree renders tokens itself.
 */
import { Tokenizer } from './Tokenizer';
import { defaultOptions } from './options';
import { other, block, inline } from './rules';
import type { Token, TokensList, Tokens } from './tokens';
import type { LexerOptions, TokenizerExtensionFunction, TokenizerStartFunction, TokenizerThis } from './options';
// Type-only, and the one place the engine names an extension: the footnote
// tables are per-DOCUMENT state, so they belong on the Lexer instance rather
// than in a module-level map that would leak across documents.
import type { FootnoteMaps } from '../extensions/footnotes';
import type { DocumentScope } from '../documentScope';

export class Lexer {
  tokens: TokensList;
  options: LexerOptions;
  state: {
    inLink: boolean;
    /**
     * Lexing the text of a link token being built. A link in it would nest
     * anchors, so no tokenizer builds one while this is set. Unlike
     * `inLink`, a raw `<a>` or `</a>` tag does not change it.
     */
    inLinkText: boolean;
    /**
     * A Markdown link was met in the link text being lexed, built in an
     * image label or refused while `inLinkText` was set. CommonMark's
     * innermost link wins: that text's own link is then not a link.
     */
    linkInText: boolean;
    inRawBlock: boolean;
    /** Lexing a text run of HTML (`htmlTextTokens`), which holds no Markdown. */
    inHtmlText: boolean;
    top: boolean;
  };

  /**
   * Inline work deferred by `inline()`, drained at the end of `lex()`.
   *
   * marked declares this private. It is public here because the list
   * extension's `tokenizeListItemContent` has to REWIND it: `blockTokens`
   * only ever appends inline work, so a discarded first pass would otherwise
   * leave entries queued against text no token holds any more.
   */
  inlineQueue: { src: string, tokens: Token[] }[];

  /**
   * The per-document footnote tables, owned by the footnote extension.
   * `null` until a `[^label]` ref or definition is seen — and stays null for
   * component rendering, where the Streamdown context owns them instead.
   */
  footnotes: FootnoteMaps | null = null;

  /**
   * What the rest of the document defines, when this Lexer lexes one block
   * of it (`../documentScope.ts` installs it), and the keys of it this run
   * read. A block's output may change only when a read key resolves
   * differently.
   */
  document: DocumentScope | null = null;
  documentReads: Set<string> | null = null;

  /** The `[center]` / `[right]` alignment in force at the current top-level
   * position (extensions/align.ts); seed it to lex from inside one. */
  align: 'center' | 'right' | null = null;

  /** How many `blockTokens` calls are running: 1 at the top level. */
  blockDepth = 0;
  /** Set by `loneParagraph`: the top-level block loop ends once its tokens
   * cannot be a single paragraph. */
  private loneParagraphProbe = false;

  /**
   * The block run in progress: the source of the innermost running
   * `blockTokens` call and the offset its loop has reached. While the loop
   * asks block extensions, the `src` they get is `src.slice(at)`; a
   * tokenizer that asks outside the loop (`blockExtensionToken`) sees null.
   * A rule that scans ahead keys what it learned on this object, so later
   * positions of the same run reuse it.
   */
  blockRun: BlockRun | null = null;

  /**
   * The inline run in progress: the source of the innermost running
   * `inlineTokens` call, its `maskInline` mask and the offset its loop has
   * reached, so an inline extension's `src` is `src.slice(at)`. A delimiter
   * rule that pairs runs reads the whole run here once
   * (extensions/strikethrough.ts). Null outside an `inlineTokens` loop.
   */
  inlineRun: InlineRun | null = null;

  /**
   * Owned per Lexer, never cached on the shared options object. marked
   * stored the tokenizer on `options` and re-pointed `tokenizer.lexer`
   * at construction, so two Lexers sharing one cached options bag shared
   * one Tokenizer — and constructing the second mid-lex of the first
   * corrupted the first's `tokenizer.lexer` back-pointer (upstream fixed
   * the symptom per-call in 17.0.5 f3a3ec05; owning the instance removes
   * the shared field instead). A Tokenizer is three assignments; the
   * rules tables it points at are module constants.
   */
  readonly tokenizer: Tokenizer;

  /**
   * The receiver every extension tokenizer and start function is invoked
   * with. Allocated once per Lexer: marked built a fresh `{ lexer: this }`
   * for every extension candidate at every token position.
   */
  private readonly tokenizerContext: TokenizerThis;

  constructor(options?: LexerOptions) {
    // TokenList cannot be created in one go
    this.tokens = [] as unknown as TokensList;
    this.tokens.links = Object.create(null);
    this.options = options || defaultOptions;
    this.tokenizer = new Tokenizer();
    this.tokenizer.options = this.options;
    this.tokenizer.lexer = this;
    this.tokenizerContext = { lexer: this };
    this.inlineQueue = [];
    this.state = {
      inLink: false,
      inLinkText: false,
      linkInText: false,
      inRawBlock: false,
      inHtmlText: false,
      top: true,
    };

    const rules = {
      other,
      block: block.normal,
      inline: inline.normal,
    };

    if (this.options.pedantic) {
      rules.block = block.pedantic;
      rules.inline = inline.pedantic;
    } else if (this.options.gfm) {
      rules.block = block.gfm;
      if (this.options.breaks) {
        rules.inline = inline.breaks;
      } else {
        rules.inline = inline.gfm;
      }
    }
    this.tokenizer.rules = rules;
  }

  /** Guards the one-shot contract below. */
  private lexed = false;

  /**
   * Preprocessing. One-shot: a Lexer is a per-document object (every
   * caller in this tree constructs a fresh one), and none of the
   * constructor-created state — `tokens`, the links table, `state` —
   * is reset here, so a second call would append the second document
   * into the first's token list and let its reference definitions
   * resolve across documents. Refused instead of documented.
   */
  lex(src: string) {
    if (this.lexed) {
      throw new Error('Lexer.lex is one-shot: construct a new Lexer per document');
    }
    this.lexed = true;
    src = src.replace(other.carriageReturn, '\n');

    this.blockTokens(src, this.tokens);

    for (let i = 0; i < this.inlineQueue.length; i++) {
      const next = this.inlineQueue[i];
      this.inlineTokens(next.src, next.tokens);
    }
    this.inlineQueue = [];

    return this.tokens;
  }

  /**
   * Lexing
   */
  blockTokens(src: string, tokens?: Token[], lastParagraphClipped?: boolean): Token[];
  blockTokens(src: string, tokens?: TokensList, lastParagraphClipped?: boolean): TokensList;
  blockTokens(src: string, tokens: Token[] = [], lastParagraphClipped = false) {
    if (this.options.pedantic) {
      src = src.replace(other.tabCharGlobal, '    ').replace(other.spaceLine, '');
    }

    this.blockDepth++;
    const outer = this.blockRun;
    const run: BlockRun = { src, at: 0 };
    this.blockRun = run;
    try {
      return this.blockTokensAtDepth(src, tokens, lastParagraphClipped, run);
    } finally {
      this.blockDepth--;
      this.blockRun = outer;
    }
  }

  /**
   * `src` as a paragraph token when it lexes as exactly one, else undefined.
   * The lex stops at the first token that rules that out, so a long run of
   * other blocks costs one token. For a throwaway Lexer: the inline work it
   * queues is not run.
   */
  loneParagraph(src: string): Tokens.Paragraph | undefined {
    this.loneParagraphProbe = true;
    const tokens = this.blockTokens(src, []);
    const only = tokens.length === 1 ? tokens[0] : undefined;
    return only?.type === 'paragraph' && only.raw === src ? (only as Tokens.Paragraph) : undefined;
  }

  /**
   * The token a block extension claims at the start of `src`, if any. The
   * block loop asks this first at every position; a tokenizer that builds a
   * block token outside that loop asks it too, so the token is the one the
   * loop would have produced there.
   */
  blockExtensionToken(src: string, tokens: Token[]): Tokens.Generic | undefined {
    const run = this.blockRun;
    this.blockRun = null;
    try {
      return this.claimBlock(src, tokens);
    } finally {
      this.blockRun = run;
    }
  }

  private claimBlock(src: string, tokens: Token[]): Tokens.Generic | undefined {
    const blockExtensions = this.options.extensions?.block;
    if (!blockExtensions) return undefined;
    for (let i = 0; i < blockExtensions.length; i++) {
      const token = blockExtensions[i].call(this.tokenizerContext, src, tokens);
      if (token) return token;
    }
    return undefined;
  }

  private blockTokensAtDepth(src: string, tokens: Token[], lastParagraphClipped: boolean, run: BlockRun) {
    const startBlock = this.options.extensions?.startBlock;

    while (src) {
      let token: Tokens.Generic | undefined;
      run.at = run.src.length - src.length;
      if (this.loneParagraphProbe && this.blockDepth === 1 && tokens.length > 0
        && (tokens.length > 1 || tokens[0].type !== 'paragraph')) break;

      // extensions
      if ((token = this.claimBlock(src, tokens))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // newline
      if ((token = this.tokenizer.space(src))) {
        src = src.substring(token.raw.length);
        const lastToken = tokens.at(-1);
        if (token.raw.length === 1 && lastToken !== undefined) {
          // if there's a single \n as a spacer, it's terminating the last line,
          // so move it there so that we don't get unnecessary paragraph tags
          lastToken.raw += '\n';
        } else {
          tokens.push(token);
        }
        continue;
      }

      // code
      if ((token = this.tokenizer.code(src))) {
        src = src.substring(token.raw.length);
        const lastToken = tokens.at(-1);
        // An indented code block cannot interrupt a paragraph.
        if (lastToken?.type === 'paragraph' || lastToken?.type === 'text') {
          lastToken.raw += (lastToken.raw.endsWith('\n') ? '' : '\n') + token.raw;
          lastToken.text += '\n' + token.text;
          this.inlineQueue.at(-1)!.src = lastToken.text;
        } else {
          tokens.push(token);
        }
        continue;
      }

      // fences
      if ((token = this.tokenizer.fences(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // heading
      if ((token = this.tokenizer.heading(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // hr
      if ((token = this.tokenizer.hr(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // blockquote
      if ((token = this.tokenizer.blockquote(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // list
      if ((token = this.tokenizer.list(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // html
      if ((token = this.tokenizer.html(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // def
      if ((token = this.tokenizer.def(src))) {
        src = src.substring(token.raw.length);
        const lastToken = tokens.at(-1);
        if (lastToken?.type === 'paragraph' || lastToken?.type === 'text') {
          lastToken.raw += (lastToken.raw.endsWith('\n') ? '' : '\n') + token.raw;
          lastToken.text += '\n' + token.raw;
          this.inlineQueue.at(-1)!.src = lastToken.text;
        } else if (!this.tokens.links[token.tag]) {
          this.tokens.links[token.tag] = {
            href: token.href,
            title: token.title,
          };
          tokens.push(token);
        }
        continue;
      }

      // table (gfm)
      if ((token = this.tokenizer.table(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // lheading
      if ((token = this.tokenizer.lheading(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // top-level paragraph
      // prevent paragraph consuming extensions by clipping 'src' to extension start
      // DIVERGENCE: only where a paragraph is built. Below the top level each
      // line is a `text` token, and scanning the start functions to the end of
      // the run from every line is quadratic in its length.
      let cutSrc = src;
      if (startBlock && this.state.top) {
        let startIndex = Infinity;
        const tempSrc = src.slice(1);
        for (let i = 0; i < startBlock.length; i++) {
          const tempStart = startBlock[i].call(this.tokenizerContext, tempSrc);
          if (typeof tempStart === 'number' && tempStart >= 0) {
            startIndex = Math.min(startIndex, tempStart);
          }
        }
        if (startIndex < Infinity && startIndex >= 0) {
          cutSrc = src.substring(0, startIndex + 1);
        }
      }
      if (this.state.top && (token = this.tokenizer.paragraph(cutSrc))) {
        const lastToken = tokens.at(-1);
        if (lastParagraphClipped && lastToken?.type === 'paragraph') {
          lastToken.raw += (lastToken.raw.endsWith('\n') ? '' : '\n') + token.raw;
          lastToken.text += '\n' + token.text;
          this.inlineQueue.pop();
          this.inlineQueue.at(-1)!.src = lastToken.text;
        } else {
          tokens.push(token);
        }
        lastParagraphClipped = cutSrc.length !== src.length;
        src = src.substring(token.raw.length);
        continue;
      }

      // text
      if ((token = this.tokenizer.text(src))) {
        src = src.substring(token.raw.length);
        const lastToken = tokens.at(-1);
        if (lastToken?.type === 'text') {
          lastToken.raw += (lastToken.raw.endsWith('\n') ? '' : '\n') + token.raw;
          lastToken.text += '\n' + token.text;
          this.inlineQueue.pop();
          this.inlineQueue.at(-1)!.src = lastToken.text;
        } else {
          tokens.push(token);
        }
        continue;
      }

      if (src) {
        const errMsg = 'Infinite loop on byte: ' + src.charCodeAt(0);
        if (this.options.silent) {
          console.error(errMsg);
          break;
        } else {
          throw new Error(errMsg);
        }
      }
    }

    this.state.top = true;
    return tokens;
  }

  inline(src: string, tokens: Token[] = []) {
    this.inlineQueue.push({ src, tokens });
    return tokens;
  }

  /**
   * `src` as the delimiter rules read it, one character for each: reference
   * links, escaped characters, links, code spans and HTML tags masked, so a
   * delimiter inside one is not seen. `emStrong` and the forge
   * strikethrough (extensions/strikethrough.ts) read delimiter runs here.
   */
  maskInline(src: string): string {
    let maskedSrc = src;

    // PORTED from marked 18.0.7 (#4017) and 18.0.11 (#4040): all three masks
    // below were `while (exec)` loops that rebuilt the whole string per match,
    // which is O(n^2) in the number of masked spans. Each mask is
    // length-preserving, so a single `replace` produces the identical string.
    // The reflink preamble additionally allocated an `Object.keys` array per
    // call — on prose with no `[` at all, for a table that is usually empty.

    // Mask out reflinks. `in`, not `Object.hasOwn`: a block's table
    // inherits the document's definitions (../documentScope.ts).
    if (this.tokens.links && src.includes('[')) {
      maskedSrc = maskedSrc.replace(this.tokenizer.rules.inline.reflinkSearch, match0 =>
        match0.slice(match0.lastIndexOf('[') + 1, -1) in this.tokens.links
          ? '[' + 'a'.repeat(match0.length - 2) + ']'
          : match0);
    }

    // Mask out escaped characters.
    // PORTED from marked 18.0.10 (#4044): every mask must keep the length it
    // replaces, because `emStrong` lines `maskedSrc` up with `src` by slicing
    // from the end. `anyPunctuation` matches unicode punctuation, so an
    // escaped astral character is 3 code units, not 2.
    maskedSrc = maskedSrc.replace(this.tokenizer.rules.inline.anyPunctuation, match0 => '+'.repeat(match0.length));

    // Mask out other blocks
    maskedSrc = maskedSrc.replace(this.tokenizer.rules.inline.blockSkip, (match0, _link, context) => {
      const offset = context ? context.length : 0;
      return match0.slice(0, offset) + '[' + 'a'.repeat(match0.length - offset - 2) + ']';
    });
    return maskedSrc;
  }

  /**
   * Lexing/Compiling
   */
  inlineTokens(src: string, tokens: Token[] = []): Token[] {
    // String with links masked to avoid interference with em and strong
    const maskedSrc = this.maskInline(src);
    const outer = this.inlineRun;
    const run: InlineRun = { src, masked: maskedSrc, at: 0 };
    this.inlineRun = run;
    try {
      return this.inlineTokensInRun(src, tokens, run);
    } finally {
      this.inlineRun = outer;
    }
  }

  private inlineTokensInRun(src: string, tokens: Token[], run: InlineRun): Token[] {
    const maskedSrc = run.masked;
    const inlineExtensions = this.options.extensions?.inline;
    const startInline = this.options.extensions?.startInline;

    let keepPrevChar = false;
    let prevChar = '';
    while (src) {
      run.at = run.src.length - src.length;
      if (!keepPrevChar) {
        prevChar = '';
      }
      keepPrevChar = false;

      let token: Tokens.Generic | undefined;

      // extensions
      if (inlineExtensions) {
        let matched = false;
        for (let i = 0; i < inlineExtensions.length; i++) {
          if ((token = inlineExtensions[i].call(this.tokenizerContext, src, tokens))) {
            src = src.substring(token.raw.length);
            tokens.push(token);
            matched = true;
            break;
          }
        }
        if (matched) {
          continue;
        }
      }

      // escape
      if ((token = this.tokenizer.escape(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // tag
      if ((token = this.tokenizer.tag(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // link
      if ((token = this.tokenizer.link(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // reflink, nolink
      if ((token = this.tokenizer.reflink(src, this.tokens.links))) {
        src = src.substring(token.raw.length);
        const lastToken = tokens.at(-1);
        if (token.type === 'text' && lastToken?.type === 'text') {
          lastToken.raw += token.raw;
          lastToken.text += token.text;
        } else {
          tokens.push(token);
        }
        continue;
      }

      // em & strong
      if ((token = this.tokenizer.emStrong(src, maskedSrc, prevChar))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // code
      if ((token = this.tokenizer.codespan(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // br
      if ((token = this.tokenizer.br(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // del (gfm)
      if ((token = this.tokenizer.del(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // autolink
      if (!this.state.inLinkText && (token = this.tokenizer.autolink(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // url (gfm)
      if (!this.state.inLink && (token = this.tokenizer.url(src))) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // text
      // prevent inlineText consuming extensions by clipping 'src' to extension start
      let cutSrc = src;
      if (startInline) {
        let startIndex = Infinity;
        const tempSrc = src.slice(1);
        for (let i = 0; i < startInline.length; i++) {
          const tempStart = startInline[i].call(this.tokenizerContext, tempSrc);
          if (typeof tempStart === 'number' && tempStart >= 0) {
            startIndex = Math.min(startIndex, tempStart);
          }
        }
        if (startIndex < Infinity && startIndex >= 0) {
          cutSrc = src.substring(0, startIndex + 1);
        }
      }
      if ((token = this.tokenizer.inlineText(cutSrc))) {
        src = src.substring(token.raw.length);
        if (token.raw.slice(-1) !== '_') { // Track prevChar before string of ____ started
          prevChar = token.raw.slice(-1);
        }
        keepPrevChar = true;
        const lastToken = tokens.at(-1);
        if (lastToken?.type === 'text') {
          lastToken.raw += token.raw;
          lastToken.text += token.text;
        } else {
          tokens.push(token);
        }
        continue;
      }

      if (src) {
        const errMsg = 'Infinite loop on byte: ' + src.charCodeAt(0);
        if (this.options.silent) {
          console.error(errMsg);
          break;
        } else {
          throw new Error(errMsg);
        }
      }
    }

    return tokens;
  }

  /**
   * `src`, a text run of HTML, as the tokens `extensions` claim in it and
   * text between; null when none claims anything. HTML text holds no
   * Markdown, so no grammar rule runs, and `state.inHtmlText` is set for an
   * extension that reads such text by another rule. Unless `linkable`,
   * `state.inLink` is set too. Each extension's next start is scanned for
   * once and kept until the run passes it, so the cost is linear in `src`.
   */
  htmlTextTokens(src: string, extensions: readonly HtmlTextExtension[], linkable: boolean): Token[] | null {
    const tokens: Token[] = [];
    const state = this.state;
    const outer = { inLink: state.inLink, inHtmlText: state.inHtmlText, inlineRun: this.inlineRun };
    state.inHtmlText = true;
    state.inLink = !linkable;
    // Its extensions read `src`, not an inline run.
    this.inlineRun = null;
    try {
      // Where each extension may claim next, as an index into the run.
      const next = extensions.map(() => -1);
      let at = 0;
      let claimed = false;
      while (src) {
        let token: Tokens.Generic | undefined;
        for (const extension of extensions) {
          if ((token = extension.tokenizer?.call(this.tokenizerContext, src, tokens))) break;
        }
        if (token) {
          src = src.substring(token.raw.length);
          at += token.raw.length;
          tokens.push(token as Token);
          claimed = true;
          continue;
        }
        let end = at + src.length;
        for (let i = 0; i < extensions.length; i++) {
          if (next[i] <= at) {
            const start = extensions[i].start?.call(this.tokenizerContext, src.slice(1));
            next[i] = typeof start === 'number' && start >= 0 ? at + 1 + start : Infinity;
          }
          end = Math.min(end, next[i]);
        }
        const text = src.slice(0, end - at);
        src = src.substring(text.length);
        at = end;
        const last = tokens.at(-1);
        if (last?.type === 'text') {
          last.raw += text;
          last.text += text;
        } else {
          tokens.push({ type: 'text', raw: text, text });
        }
      }
      return claimed ? tokens : null;
    } finally {
      state.inLink = outer.inLink;
      state.inHtmlText = outer.inHtmlText;
      this.inlineRun = outer.inlineRun;
    }
  }
}

/** `Lexer.blockRun`. */
export interface BlockRun {
  readonly src: string;
  at: number;
}

/** `Lexer.inlineRun`. */
export interface InlineRun {
  readonly src: string;
  readonly masked: string;
  at: number;
}

/** An inline extension as `htmlTextTokens` runs it. */
export interface HtmlTextExtension {
  tokenizer?: TokenizerExtensionFunction;
  start?: TokenizerStartFunction;
}
