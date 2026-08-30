import type { Extension } from '../index';
import type { Tokenizer, Tokens } from 'marked';

type variantType = 'note' | 'tip' | 'important' | 'warning' | 'caution';
import { Lexer } from 'marked';
const variants: variantType[] = ['note', 'tip', 'important', 'warning', 'caution'];
export function createSyntaxPattern(type: variantType): string {
    return `^\\s*[\\*_]*\\[!${type.toUpperCase()}\\][\\*_]*\\s*`;
}
// Precomputed once per variant instead of recompiling on every blockquote:
//  - syntax: detects the `[!NOTE]` marker (case-insensitive)
//  - strip:  removes the marker (global) when building the alert token
const VARIANT_PATTERNS = variants.map((type) => ({
    type,
    syntax: new RegExp(createSyntaxPattern(type), 'i'),
    strip: new RegExp(`[\\*_]*\\[!${type.toUpperCase()}\\][\\*_]*`, 'g')
}));
// Cheap pre-gate. marked calls every registered block extension at every
// block position, so the per-call Lexer below must not be built for the
// overwhelming majority of positions that cannot possibly be a blockquote.
// Mirrors marked's own `rules.other.blockquoteStart`.
const BLOCKQUOTE_OPEN = /^ {0,3}>/;
// Trailing newlines are `rtrim`ed off the block rule's match before the
// tokenizer walks its lines, so they are never part of what it consumes.
// Scanned rather than `match.replace(/\n+$/, '')`: this runs once per
// blockquote per FULL LEX, and the volatile tail re-lexes on every reveal
// tick, so the discarded copy of every quoted block was pure garbage per
// frame. `\n` is charCode 10, and the old regex matched nothing else.
const trimmedNewlineEnd = (value: string): number => {
    let end = value.length;
    while (end > 0 && value.charCodeAt(end - 1) === 10)
        end -= 1;
    return end;
};
/**
 * The prefix of `src` that marked's blockquote tokenizer consumed.
 *
 * marked rebuilds a blockquote's `raw` by re-joining the lines it walked,
 * and two of its continuation branches splice the INNER (marker-stripped)
 * token's raw back into the OUTER one. So `raw` can come back holding bytes
 * the source never had at that offset (`">  - - \n$$\n"` -> `">  - -\n\n\n$$"`)
 * and even longer than the source itself (`"> - a\n[^a]:"` -> `"> - a\n[^a]:\n"`).
 * Both shapes terminate, which is why they went unnoticed: marked only ever
 * reads `raw.length` to advance, so the LENGTH is the consumption even when
 * the bytes are not, and the over-run is one byte past the end of the block
 * rule's own match — nothing crashes, nothing is swallowed.
 *
 * What it breaks is the contract every incremental path in this pipeline is
 * built on: a block token's `raw` names the bytes it consumed. That is what
 * `parseBlocks`' contiguity sum and `incrementalLex`'s raw-offset arithmetic
 * check their offsets against. A raw longer than the source makes such a sum
 * meaningless; a same-length-different-bytes raw passes it while the cached
 * block's raw no longer describes the source it came from.
 *
 * Report the prefix that `raw.length` names — the consumption is unchanged —
 * clamped to the block rule's own match minus the newlines the tokenizer
 * rtrims. The tokenizer cannot read past its match, so an over-long `raw` is
 * always the splice bug and never a longer read.
 */
const consumedPrefix = (src: string, match: string, raw: string): string => src.slice(0, Math.min(raw.length, trimmedNewlineEnd(match)));
export const markedAlert: Extension = {
    name: 'alert',
    level: 'block',
    tokenizer(src) {
        if (!BLOCKQUOTE_OPEN.test(src))
            return undefined;
        // Blockquote-scoped Lexer, thrown away with the call.
        //
        // `Tokenizer.blockquote` lexes the quoted body through `this.lexer`,
        // which pushes onto that lexer's `inlineQueue`, its `tokens.links`
        // table and its footnote maps. Upstream hung a MODULE-LEVEL Lexer
        // here and reused it for every blockquote of every document: nothing
        // drains an `inlineQueue` that `Lexer.lex()` did not create, so it
        // grew for the lifetime of the page (16,511 retained entries over one
        // test corpus), and the link/footnote tables carried reference
        // definitions from one document into the next.
        //
        // A per-call lexer makes that structurally impossible — there is no
        // field left to remember to reset when marked grows another one —
        // and it costs a handful of assignments: the rules tables it points
        // at are marked's module constants, shared by every instance.
        const lexer = new Lexer({ gfm: true });
        const tokenizer = lexer.options.tokenizer;
        const activeTokenizer = this.lexer.options.tokenizer;
        // `new Lexer()` always installs both; the guard keeps the reads total
        // rather than asserting through marked's optional declaration.
        if (!tokenizer || !activeTokenizer)
            return undefined;
        const cap = tokenizer.rules.block.blockquote.exec(src);
        if (!cap)
            return undefined;
        const blockquoteToken = tokenizer.blockquote(src);
        if (!blockquoteToken)
            return undefined;
        blockquoteToken.raw = consumedPrefix(src, cap[0], blockquoteToken.raw);
        // The body is re-lexed through the ACTIVE lexer (below), so the
        // alert's children — and their inline queue entries — belong to the
        // document being parsed, not to the scratch lexer above.
        processAlertToken(blockquoteToken, activeTokenizer);
        return blockquoteToken;
    }
};
export function processAlertToken(token: Tokens.Blockquote, tokenizer: Tokenizer): void {
    const matched = VARIANT_PATTERNS.find((v) => v.syntax.test(('text' in token && token.text) || ''));
    if (!matched) {
        Object.assign(token, {
            tokens: token.tokens
                .map((token) => {
                return tokenizer.lexer.blockTokens(token.raw, [])[0];
            })
                .filter(Boolean)
        });
        return;
    }
    const matchedVariant = matched.type;
    const alertPattern = matched.strip;
    const tokens = token.tokens
        .map((token) => {
        let cleanedRaw = token.raw;
        // Remove alert markers with any markdown formatting (asterisks/underscores)
        cleanedRaw = cleanedRaw.replaceAll(alertPattern, '').trim();
        return tokenizer.lexer.blockTokens(cleanedRaw, [])[0];
    })
        .filter(Boolean);
    Object.assign(token, {
        type: 'alert',
        variant: matchedVariant,
        tokens,
        text: token.text.replace(alertPattern, '').trim()
    });
}

export type AlertToken = {
    type: 'alert';
    variant: variantType;
    raw: string;
    text: string;
};
