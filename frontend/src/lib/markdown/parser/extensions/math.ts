import type { Extension } from '../index';
// Math parsing rules
// Block math: three alternatives, tried left-to-right (lazy quantifiers + leftmost-first).
//   Alt 1 (group 2): $$\nCONTENT\n$$  — canonical multiline form.
//   Alt 2 (group 3): $$CONTENT$$       — single-line, content excludes $ and \n.
//   Alt 3 (group 4): $$CONTENT$$       — content may include $ AND \n. Closing $$ must
//                                       be followed by \n or end-of-string so we don't
//                                       over-match adjacent inline $$X$$ on the same line
//                                       (those still go through Alt 2 with its trailing
//                                       whitespace lookahead, which fires first).
// Alt 3 was added to handle LLM-emitted matrices like `$$ \begin{pmatrix}...\n...\n$$`,
// which open with a space (not newline) and contain internal newlines — neither legacy
// alternative accepted that shape, so the block fell through to plain paragraph rendering
// (the user-visible "math starts to render then turns back into raw markdown" symptom
// during streaming, where the inline rule briefly matched a single-line prefix before
// the first newline arrived).
const blockRule = /^(\$\$)(?:\n((?:\\[\s\S]|[^\\])+?)\n\1(?:\n|$)|([^$\n]+?)\1(?=\s|$|$)|([\s\S]+?)\1(?=\n|$))/;
// Inline math: handles both single ($) and double ($$) dollar delimiters
// Avoids matching currency by checking context and requiring proper content
const inlineRule = /^(\${1,2})(?!\$)((?:[^$\n]|\\\$)*?)\1(?!\d)/;
// A single `$…$` span is real inline math only when its closing `$` is
// followed by whitespace/punctuation/end-of-line AND its content holds no
// backtick. Agent prose is dense with `$`-prefixed tokens (`$ref`, `$PATH`,
// `$HOME`, jQuery `$el`) and inline code spans (`` `$ref` ``, `` `$` ``); a
// bare `$` opens a span that closes on the `$` of a *later* such token,
// swallowing the prose between into a KaTeX render (serif font, collapsed
// whitespace — the reported "`$ref` … `$ref`" corruption). Two independent
// tells that a span is prose, not math:
//   (a) the closing `$` abuts an identifier char — `$PATH and $HOME`, or a
//       span that closes on a bare `$ref`/`$foo` in running text.
//   (b) the captured content contains a backtick — the closing `$` lives
//       inside a code span (`` `$` `` / `` `$ref` ``), so the content runs
//       up to and includes that span's opening backtick. Real inline math
//       never contains a backtick, and (a) alone misses this because the
//       char *after* the close is the code span's backtick, not an
//       identifier (the `` `$` `` … `` `$` `` case the word-char-only guard
//       shipped without — observed rendering "ref cycles" closing on the"
//       as serif math).
// Caller gates on single `$`; `$$…$$` carries explicit math intent and is
// exempt, mirroring the currency guard's scoping. The identifier class
// matches this patch's parse-incomplete-markdown.js prev-char check;
// `inlineRule`'s `(?!\d)` already bars a trailing digit, so (a) effectively
// adds letters/underscore. `charAfterClose` is `src.charAt(match[0].length)`
// for whichever string `match` came from; `content` is `match[2]`. Known
// residual: pure bare-`$` prose with no backtick whose closer is
// whitespace/punctuation (e.g. "$foo and bar$ here") still parses as math —
// rare in agent output. Regression: AssistantMessage.test.ts.
const singleDollarLooksLikeProse = (content: string, charAfterClose: string): boolean =>
    /[\p{L}\p{N}_]/u.test(charAfterClose) || content.includes('`');
// Enhanced currency detection patterns
const currencyPatterns = {
    // Simple price patterns: $123, $123.45, $1,234.56
    simplePrice: /^\d{1,3}(?:,\d{3})*(?:\.\d{2})?$/,
    // Multiple prices or numbers: "123, 456", "123.45, 678.90", "1,234;5"
    // A comma always separates two numbers and a dot always continues one,
    // so each character has one reading. Letting a comma also continue a
    // number matches the same strings but backtracks exponentially on a
    // near miss such as "1,1,1,...,1x".
    multipleNumbers: /^\d+(?:\.\d+)*(?:\s*[,;]\s*\d+(?:\.\d+)*)+$/,
    // Price ranges: "123-456", "123 - 456", "123 to 456"
    priceRange: /^\d+(?:\.\d{2})?\s*(?:-|to|or)\s*\d+(?:\.\d{2})?$/i,
    // Common currency words nearby (check surrounding context)
    currencyContext: /(?:price|cost|dollar|euro|pound|yen|currency|pay|buy|sell|expensive|cheap)/i
};
export const markedMath: Extension[] = [
    {
        name: 'math',
        level: 'block',
        tokenizer(src) {
            if (src.charCodeAt(0) !== 36 || src.charCodeAt(1) !== 36)
                return undefined;
            const match = src.match(blockRule);
            if (match) {
                // match[2] = newline-delimited, match[3] = single-line no-newline,
                // match[4] = flexible form (content may contain newlines / inner $).
                const content = (match[2] || match[3] || match[4]).trim();
                return {
                    type: 'math',
                    isInline: false,
                    displayMode: true,
                    raw: match[0],
                    text: content
                };
            }
        }
    },
    {
        name: 'math',
        level: 'inline',
        start(src) {
            let index = 0;
            let searchSrc = src;
            while (searchSrc) {
                const dollarIndex = searchSrc.indexOf('$');
                if (dollarIndex === -1) {
                    return;
                }
                const currentIndex = index + dollarIndex;
                const possibleMath = src.substring(currentIndex);
                // Check if this could be math (not currency)
                const match = possibleMath.match(inlineRule);
                if (match) {
                    const content = match[2];
                    const dollarCount = match[1]; // '$' or '$$'
                    // Only apply currency detection to single dollars
                    // Double dollars ($$) indicate explicit math intent
                    if (dollarCount === '$' && isCurrencyPattern(content, src, currentIndex)) {
                        // This looks like currency with single dollars, skip it
                        index += dollarIndex + 1;
                        searchSrc = src.substring(index);
                        continue;
                    }
                    // Single-`$` prose guard (see singleDollarLooksLikeProse).
                    if (dollarCount === '$' &&
                        singleDollarLooksLikeProse(content, possibleMath.charAt(match[0].length))) {
                        index += dollarIndex + 1;
                        searchSrc = src.substring(index);
                        continue;
                    }
                    return currentIndex;
                }
                index += dollarIndex + 1;
                searchSrc = src.substring(index);
            }
        },
        tokenizer(src) {
            if (src.charCodeAt(0) !== 36)
                return undefined;
            const match = src.match(inlineRule);
            if (match) {
                const content = match[2];
                const dollarCount = match[1]; // '$' or '$$'
                const isDisplayMode = dollarCount === '$$';
                // Only apply currency detection to single dollars
                // Double dollars ($$) indicate explicit math intent
                if (dollarCount === '$' && isCurrencyPattern(content, src, 0)) {
                    // This looks like currency with single dollars, skip it
                    return;
                }
                // Single-`$` prose guard (see singleDollarLooksLikeProse).
                if (dollarCount === '$' &&
                    singleDollarLooksLikeProse(content, src.charAt(match[0].length))) {
                    return;
                }
                return {
                    type: 'math',
                    isInline: true, // Inline tokenizer always produces inline math
                    displayMode: isDisplayMode, // $$ = display mode styling, $ = inline styling
                    raw: match[0],
                    text: content.trim()
                };
            }
        }
    }
];
// ---------------------------------------------------------------------------
// Forge math
// ---------------------------------------------------------------------------

type Forge = 'github' | 'gitlab';

// A code span's content as CommonMark reads it: line endings as spaces,
// and one space off each end when both ends have one and it is not all spaces.
const codeSpanContent = (text: string): string => {
    const flat = text.replace(/\r?\n/g, ' ');
    return /^ [\s\S]*[^ ][\s\S]* $/.test(flat) ? flat.slice(1, -1) : flat;
};
// CommonMark's backslash escapes: a backslash before ASCII punctuation.
const unescapeMarkdown = (text: string): string => text.replace(/\\([!-/:-@[-`{-~])/g, '$1');
const DOUBLE_DOLLAR = /^\$\$(?!\$)((?:[^$\n]|\\\$)+?)\$\$/;

/**
 * The math a forge reads at `src`'s leading `$`, as both GitHub and GitLab
 * read it: `` $`…`$ `` (a code span between dollars); `$$…$$`, display math
 * on GitLab; and `$…$`, whose opening `$` has no space after it and whose
 * first unescaped `$` closes it when it has no space before it and no digit
 * after it, else there is no math here. GitHub reads the `$…$` content after
 * Markdown's backslash escapes; GitLab reads it as written.
 */
const forgeMathAt = (src: string, forge: Forge): MathToken | undefined => {
    const ticks = /^\$(`+)/.exec(src);
    if (ticks) {
        const run = ticks[1].length;
        for (let at = src.indexOf('`', ticks[0].length); at !== -1; at = src.indexOf('`', at)) {
            let length = 1;
            while (src[at + length] === '`')
                length++;
            if (length === run) {
                if (src[at + length] !== '$')
                    return undefined;
                const raw = src.slice(0, at + length + 1);
                return { type: 'math', raw, text: codeSpanContent(src.slice(ticks[0].length, at)), isInline: true, displayMode: false };
            }
            at += length;
        }
        return undefined;
    }
    const double = DOUBLE_DOLLAR.exec(src);
    if (double) {
        return { type: 'math', raw: double[0], text: double[1].trim(), isInline: forge === 'github', displayMode: true };
    }
    if (src.length < 3 || /\s|\$/.test(src[1]))
        return undefined;
    for (let at = 1; at < src.length; at++) {
        const char = src[at];
        if (char === '\\') {
            at++;
            continue;
        }
        if (char === '\n')
            return undefined;
        if (char !== '$')
            continue;
        if (/\s/.test(src[at - 1]) || /\d/.test(src[at + 1] ?? ''))
            return undefined;
        const content = src.slice(1, at);
        return {
            type: 'math',
            raw: src.slice(0, at + 1),
            text: forge === 'github' ? unescapeMarkdown(content) : content,
            isInline: true,
            displayMode: false
        };
    }
    return undefined;
};

// A forge surface leaves chat's inline rule out (`markedForgeGrammar`), so
// dollars this rule does not read are text.
const forgeInlineMath = (forge: Forge): Extension => ({
    name: 'forgeMath',
    level: 'inline',
    start(src) {
        const at = src.indexOf('$');
        return at === -1 ? undefined : at;
    },
    tokenizer(src) {
        if (src.charCodeAt(0) !== 36 /* $ */)
            return undefined;
        return forgeMathAt(src, forge);
    }
});

/** A fence whose info string is `math`: display math on both forges. */
const markedMathFence: Extension = {
    name: 'mathFence',
    level: 'block',
    tokenizer(src) {
        let at = 0;
        while (at < 3 && src.charCodeAt(at) === 32)
            at++;
        const first = src.charCodeAt(at);
        if (first !== 96 /* ` */ && first !== 126 /* ~ */)
            return undefined;
        const code = this.lexer.tokenizer.fences(src);
        if (!code || code.lang?.split(/\s/, 1)[0] !== 'math')
            return undefined;
        return { type: 'math', raw: code.raw, text: code.text, isInline: false, displayMode: true } satisfies MathToken;
    }
};

/** Math as GitHub reads it, for a GitHub surface. */
export const markedGithubMath: readonly Extension[] = [forgeInlineMath('github'), markedMathFence];
/** Math as GitLab reads it, for a GitLab surface. */
export const markedGitlabMath: readonly Extension[] = [forgeInlineMath('gitlab'), markedMathFence];

// Helper function to detect currency patterns
function isCurrencyPattern(content: string, fullSrc: string, dollarIndex: number): boolean {
    const trimmedContent = content.trim();
    // Check for simple price patterns
    if (currencyPatterns.simplePrice.test(trimmedContent)) {
        return true;
    }
    // Check for multiple numbers/prices pattern (like "199, 199")
    if (currencyPatterns.multipleNumbers.test(trimmedContent)) {
        return true;
    }
    // Check for price ranges
    if (currencyPatterns.priceRange.test(trimmedContent)) {
        return true;
    }
    // Check surrounding context for currency-related words
    const contextStart = Math.max(0, dollarIndex - 50);
    const contextEnd = Math.min(fullSrc.length, dollarIndex + content.length + 50);
    const context = fullSrc.substring(contextStart, contextEnd);
    if (currencyPatterns.currencyContext.test(context)) {
        // If currency context is found and content is purely numeric, likely currency
        if (/^\d+(?:[.,]\d+)*$/.test(trimmedContent)) {
            return true;
        }
    }
    // Additional check: if content is just numbers with common currency formatting
    if (/^\d{1,3}(?:,\d{3})*(?:\.\d{1,2})?$/.test(trimmedContent)) {
        return true;
    }
    return false;
}

export type MathToken = {
    type: 'math';
    raw: string;
    text: string;
    isInline: boolean;
    displayMode: boolean;
};
