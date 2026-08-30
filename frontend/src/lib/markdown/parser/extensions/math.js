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
const singleDollarLooksLikeProse = (content, charAfterClose) =>
    /[\p{L}\p{N}_]/u.test(charAfterClose) || content.includes('`');
// Enhanced currency detection patterns
const currencyPatterns = {
    // Simple price patterns: $123, $123.45, $1,234.56
    simplePrice: /^\d{1,3}(?:,\d{3})*(?:\.\d{2})?$/,
    // Multiple prices or numbers: "123, 456", "123.45, 678.90", "123 or 456"
    multipleNumbers: /^\d+(?:[.,]\d+)*(?:\s*[,;]\s*\d+(?:[.,]\d+)*)+$/,
    // Price ranges: "123-456", "123 - 456", "123 to 456"
    priceRange: /^\d+(?:\.\d{2})?\s*(?:-|to|or)\s*\d+(?:\.\d{2})?$/i,
    // Common currency words nearby (check surrounding context)
    currencyContext: /(?:price|cost|dollar|euro|pound|yen|currency|pay|buy|sell|expensive|cheap)/i
};
export const markedMath = [
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
// Helper function to detect currency patterns
function isCurrencyPattern(content, fullSrc, dollarIndex) {
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
