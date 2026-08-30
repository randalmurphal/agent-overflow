import type { Extension } from '../index';
import type { Tokenizer, Tokens } from '../engine';

type variantType = 'note' | 'tip' | 'important' | 'warning' | 'caution';
import { Lexer } from '../engine';
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
        // at are the engine's module constants, shared by every instance.
        const lexer = new Lexer({ gfm: true });
        const blockquoteToken = lexer.tokenizer.blockquote(src);
        if (!blockquoteToken)
            return undefined;
        // The body is re-lexed through the ACTIVE lexer (below), so the
        // alert's children — and their inline queue entries — belong to the
        // document being parsed, not to the scratch lexer above.
        processAlertToken(blockquoteToken, this.lexer.tokenizer);
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
