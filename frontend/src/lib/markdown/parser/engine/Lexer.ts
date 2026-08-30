/**
 * The block/inline lexer.
 *
 * Absorbed from marked 16.4.2 (`src/Lexer.ts`, MIT — see `../../LICENSE`),
 * with three classes of deliberate divergence, each commented at its site:
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
 *
 * marked's `Parser`, `Renderer`, `Hooks`, `Instance` and the `marked()`
 * façade are NOT absorbed: this tree renders tokens itself.
 */
import { Tokenizer } from './Tokenizer';
import { defaultOptions } from './options';
import { other, block, inline } from './rules';
import type { Token, TokensList, Tokens } from './tokens';
import type { LexerOptions, TokenizerThis } from './options';
// Type-only, and the one place the engine names an extension: the footnote
// tables are per-DOCUMENT state, so they belong on the Lexer instance rather
// than in a module-level map that would leak across documents.
import type { FootnoteMaps } from '../extensions/footnotes';

export class Lexer {
  tokens: TokensList;
  options: LexerOptions;
  state: {
    inLink: boolean;
    inRawBlock: boolean;
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

  private tokenizer: Tokenizer;

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
    this.options.tokenizer = this.options.tokenizer || new Tokenizer();
    this.tokenizer = this.options.tokenizer;
    this.tokenizer.options = this.options;
    this.tokenizer.lexer = this;
    this.tokenizerContext = { lexer: this };
    this.inlineQueue = [];
    this.state = {
      inLink: false,
      inRawBlock: false,
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

  /**
   * Preprocessing
   */
  lex(src: string) {
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

    const blockExtensions = this.options.extensions?.block;
    const startBlock = this.options.extensions?.startBlock;

    while (src) {
      let token: Tokens.Generic | undefined;

      // extensions
      if (blockExtensions) {
        let matched = false;
        for (let i = 0; i < blockExtensions.length; i++) {
          if (token = blockExtensions[i].call(this.tokenizerContext, src, tokens)) {
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

      // newline
      if (token = this.tokenizer.space(src)) {
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
      if (token = this.tokenizer.code(src)) {
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
      if (token = this.tokenizer.fences(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // heading
      if (token = this.tokenizer.heading(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // hr
      if (token = this.tokenizer.hr(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // blockquote
      if (token = this.tokenizer.blockquote(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // list
      if (token = this.tokenizer.list(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // html
      if (token = this.tokenizer.html(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // def
      if (token = this.tokenizer.def(src)) {
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
      if (token = this.tokenizer.table(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // lheading
      if (token = this.tokenizer.lheading(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // top-level paragraph
      // prevent paragraph consuming extensions by clipping 'src' to extension start
      let cutSrc = src;
      if (startBlock) {
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
      if (token = this.tokenizer.text(src)) {
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
   * Lexing/Compiling
   */
  inlineTokens(src: string, tokens: Token[] = []): Token[] {
    // String with links masked to avoid interference with em and strong
    let maskedSrc = src;

    // PORTED from marked 18.0.7 (#4017) and 18.0.11 (#4040): all three masks
    // below were `while (exec)` loops that rebuilt the whole string per match,
    // which is O(n^2) in the number of masked spans. Each mask is
    // length-preserving, so a single `replace` produces the identical string.
    // The reflink preamble additionally allocated an `Object.keys` array per
    // call — on prose with no `[` at all, for a table that is usually empty.

    // Mask out reflinks
    if (this.tokens.links && src.includes('[')) {
      maskedSrc = maskedSrc.replace(this.tokenizer.rules.inline.reflinkSearch, match0 =>
        Object.hasOwn(this.tokens.links, match0.slice(match0.lastIndexOf('[') + 1, -1))
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

    const inlineExtensions = this.options.extensions?.inline;
    const startInline = this.options.extensions?.startInline;

    let keepPrevChar = false;
    let prevChar = '';
    while (src) {
      if (!keepPrevChar) {
        prevChar = '';
      }
      keepPrevChar = false;

      let token: Tokens.Generic | undefined;

      // extensions
      if (inlineExtensions) {
        let matched = false;
        for (let i = 0; i < inlineExtensions.length; i++) {
          if (token = inlineExtensions[i].call(this.tokenizerContext, src, tokens)) {
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
      if (token = this.tokenizer.escape(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // tag
      if (token = this.tokenizer.tag(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // link
      if (token = this.tokenizer.link(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // reflink, nolink
      if (token = this.tokenizer.reflink(src, this.tokens.links)) {
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
      if (token = this.tokenizer.emStrong(src, maskedSrc, prevChar)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // code
      if (token = this.tokenizer.codespan(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // br
      if (token = this.tokenizer.br(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // del (gfm)
      if (token = this.tokenizer.del(src)) {
        src = src.substring(token.raw.length);
        tokens.push(token);
        continue;
      }

      // autolink
      if (token = this.tokenizer.autolink(src)) {
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
      if (token = this.tokenizer.inlineText(cutSrc)) {
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
}
