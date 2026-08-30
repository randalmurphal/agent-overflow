import type { Lexer, LexerOptions } from '../engine';
import type { Extension, GenericToken } from '../index';

export function letterToInt(letter: string): number {
    return letter.toLowerCase().charCodeAt(0) - 96;
}
const romanMap: Record<string, number> = {
    I: 1,
    V: 5,
    X: 10,
    L: 50,
    C: 100,
    D: 500,
    M: 1000
};
export function romanToInt(roman: string): number {
    roman = roman.toUpperCase();
    let total = 0;
    for (let i = 0; i < roman.length; i++) {
        const current = romanMap[roman[i]];
        const next = romanMap[roman[i + 1]];
        total += next && current < next ? -current : current;
    }
    return total;
}
// Regular expression patterns for list detection
export const romanUpper = '(?:C|XC|L?X{0,3}(?:IX|IV|V?I{0,3}))';
export const romanLower = '(?:c|xc|l?x{0,3}(?:ix|iv|v?i{0,3}))';
// Fixed regex pattern - carefully balanced parentheses
export const bulletPattern = `(?:[*+-]|(?:\\d{1,9}|[a-zA-Z]|${romanUpper}|${romanLower})[.)])`;
export const rule = `^( {0,3}${bulletPattern})([ \\t][^\\n]*|[ \\t])?(?:\\n|$)`;
// --- Precompiled regexes ---------------------------------------------------
// These were previously rebuilt on every tokenizer call and, for the boundary
// set, on every item iteration. They are stateless (no `g` flag) so they are
// safe to share. This is the bulk of the per-chunk list cost.
const RULE_RE = new RegExp(rule);
const ROMAN_UPPER_RE = new RegExp(`^${romanUpper}[.)]$`);
const ROMAN_LOWER_RE = new RegExp(`^${romanLower}[.)]$`);
const LOWER_ALPHA_RE = /^[a-z][.)]$/;
const UPPER_ALPHA_RE = /^[A-Z][.)]$/;
// The per-item boundary regexes vary only by the indent clamp (0..3); build the
// four variants once at module load and index into them.
function buildBoundaryRegexes(maxIndent: number) {
    return {
        nextBullet: new RegExp(`^ {0,${maxIndent}}(?:[*+-]|(?:\\d{1,9}|[a-zA-Z]|${romanUpper}|${romanLower})[.)])((?:[ \t][^\\n]*)?(?:\\n|$))`),
        hr: new RegExp(`^ {0,${maxIndent}}((?:- *){3,}|(?:_ *){3,}|(?:\\* *){3,})(?:\\n+|$)`),
        fences: new RegExp(`^ {0,${maxIndent}}(?:\`\`\`|~~~)`),
        heading: new RegExp(`^ {0,${maxIndent}}#`),
        html: new RegExp(`^ {0,${maxIndent}}<[a-z].*>`, 'i')
    };
}
const LIST_BOUNDARY_TABLE = [0, 1, 2, 3].map(buildBoundaryRegexes);
// itemRegex depends only on `bull`, which has a small finite set of shapes;
// cache the compiled instances instead of recompiling per tokenizer call.
const itemRegexCache = new Map<string, RegExp>();
function getItemRegex(bull: string): RegExp {
    let re = itemRegexCache.get(bull);
    if (!re) {
        re = new RegExp(`^( {0,3}${bull})([\t ][^\\n]*|[\t ])?(?:\\n|$)`);
        itemRegexCache.set(bull, re);
    }
    return re;
}
// --- Marker-line alignment is not an indented code block --------------------
// CommonMark reads five or more columns between a list marker and its content
// as "list item starting with indented code", so `-     $499 per month` is a
// code block. Agent/LLM prose uses that spacing to ALIGN values in a bullet
// list, never to open a code block, and the mis-render is the loudest one
// there is: a full code card per bullet, mid-sentence.
//
// Deliberate spec deviation, scoped to exactly that shape — a `code` token
// that is the FIRST child of a list item and indented-style, which can only
// mean the code opened on the marker's own line. Everything else keeps
// CommonMark's rules, including the two forms this must NOT touch:
//   - `- item\n\n        deep indent` — code, but not the first child
//     (paragraph, space, code), so the author's indent was deliberate.
//   - `-\n\n    code` — the blank marker line closes the item; the code is a
//     sibling of the list, not a child of the item.
// Fenced code is likewise untouched: `codeBlockStyle` is only `'indented'`
// for the indented form, so a first-child ``` fence never matches.
//
// This is the one chokepoint for tokenizing a list item's content — the
// tight pass, the loose re-tokenize, and the incremental-lex tail loosener
// all route through it, so streamed and settled renders cannot disagree.
const MARKER_LINE_HAS_CONTENT = /^[^\n]*\S/;
const MULTIPLE_LINE_BREAKS = /\n.*\n/;
// The marker line's leading run is fake indentation (alignment), so remove
// exactly that much from every line — clamped per line, so a line indented
// less than the first keeps its own column. The first line lands at column 0,
// which is what makes the re-lex unable to fall back into indented code.
function stripMarkerAlignment(text: string): string {
    const lines = text.split('\n');
    // `/^ */` cannot fail; `?? 0` keeps the read total without an assertion.
    const alignment = /^ */.exec(lines[0])?.[0].length ?? 0;
    if (alignment === 0)
        return text;
    for (let i = 0; i < lines.length; i++) {
        const line = lines[i];
        let strip = 0;
        while (strip < alignment && line[strip] === ' ')
            strip++;
        lines[i] = line.slice(strip);
    }
    return lines.join('\n');
}
export function tokenizeListItemContent(lexer: Lexer, item: ListItemToken, top: boolean): void {
    const queued = lexer.inlineQueue.length;
    lexer.state.top = top;
    const tokens = lexer.blockTokens(item.text, []);
    // `tokens[0]` starts at offset 0 of the item's content, so an
    // indented-style code token there — with a non-blank first line, which a
    // blank marker line cannot produce (blockTokens emits `space` first) —
    // is precisely code opened by the marker line's own alignment. Testing
    // the TOKEN rather than the raw indentation matters: block extensions
    // tokenize ahead of marked's built-ins, so an indented first line they
    // claimed (math, mdx, …) is not this artifact and must not be dedented.
    const first = tokens[0];
    if (first !== undefined &&
        first.type === 'code' &&
        first.codeBlockStyle === 'indented' &&
        MARKER_LINE_HAS_CONTENT.test(item.text)) {
        // Re-lex the whole item with the alignment removed rather than
        // hand-building a replacement token: inline content (emphasis,
        // links, code spans) gets its real inline pass, nested lists
        // re-enter this same chokepoint, a blank line inside the run still
        // yields the several blocks the author wrote, and content after the
        // code region rejoins its first line instead of being spliced onto
        // a foreign token. The discarded pass's inline work goes with it —
        // blockTokens only ever appends to the queue.
        lexer.inlineQueue.length = queued;
        lexer.state.top = top;
        item.tokens = lexer.blockTokens(stripMarkerAlignment(item.text), []);
        return;
    }
    item.tokens = tokens;
}
function finalizeListSource(list: ListToken): void {
    if (list.tokens.length === 0)
        return;
    // Trim trailing newline from last item
    const lastItem = list.tokens[list.tokens.length - 1];
    lastItem.raw = lastItem.raw.trimEnd();
    if (lastItem.text !== undefined)
        lastItem.text = lastItem.text.trimEnd();
    list.raw = list.raw.trimEnd();
}
function finalizeList(list: ListToken, lexer: Lexer): void {
    finalizeListSource(list);
    // Handle child tokens
    for (const item of list.tokens) {
        tokenizeListItemContent(lexer, item, false);
        // A blank line inside a single item also makes the list loose
        if (!list.loose) {
            for (const token of item.tokens) {
                if (token.type === 'space' && MULTIPLE_LINE_BREAKS.test(token.raw)) {
                    list.loose = true;
                    break;
                }
            }
        }
    }
    // Mark list as loose if needed and re-tokenize items as block content so
    // their text becomes paragraph tokens instead of inline text
    if (list.loose) {
        for (const item of list.tokens) {
            item.loose = true;
            tokenizeListItemContent(lexer, item, true);
        }
    }
}
function escapeForRegex(s: string): string {
    return s.replace(/[\\^$.*+?()[\]{}|]/g, '\\$&');
}
function firstLineOf(source: string): string {
    const newline = source.indexOf('\n');
    return newline === -1 ? source : source.slice(0, newline);
}
function listMarkerMayStart(source: string): boolean {
    let index = 0;
    while (index < source.length && index < 4 && source.charCodeAt(index) === 32)
        index++;
    if (index > 3 || index >= source.length)
        return false;
    const marker = source.charCodeAt(index);
    if (marker === 42 || marker === 43 || marker === 45) {
        const next = source.charCodeAt(index + 1);
        return index + 1 === source.length || next === 9 || next === 10 || next === 32;
    }
    let end = index;
    if (marker >= 48 && marker <= 57) {
        while (end < source.length && end - index < 10) {
            const code = source.charCodeAt(end);
            if (code < 48 || code > 57)
                break;
            end++;
        }
    }
    else if ((marker >= 65 && marker <= 90) || (marker >= 97 && marker <= 122)) {
        while (end < source.length) {
            const code = source.charCodeAt(end);
            if (!((code >= 65 && code <= 90) || (code >= 97 && code <= 122)))
                break;
            end++;
        }
    }
    else {
        return false;
    }
    const punctuation = source.charCodeAt(end);
    if (punctuation !== 41 && punctuation !== 46)
        return false;
    const next = source.charCodeAt(end + 1);
    return end + 1 === source.length || next === 9 || next === 10 || next === 32;
}
export function parseListSource(src: string, options: LexerOptions, sourceOnly: true): ListBoundaryToken | undefined;
export function parseListSource(src: string, options: LexerOptions, sourceOnly?: false): ListToken | undefined;
export function parseListSource(
    src: string,
    options: LexerOptions,
    sourceOnly = false
): ListToken | ListBoundaryToken | undefined {
    if (!listMarkerMayStart(src))
        return undefined;
    const originalSource = src;
    let cap = RULE_RE.exec(src);
    if (!cap)
        return undefined;
    const bullet = cap[1].trim();
    const isOrdered = bullet !== '*' && bullet !== '-' && bullet !== '+';
    let bull: string;
    let type: ListToken['listType'] = null;
    let expectedValue: number | null = null;
    // Detect list type (Roman, alphabetic, numeric)
    if (isOrdered) {
        if (ROMAN_UPPER_RE.test(bullet)) {
            type = 'upper-roman';
            bull = `${romanUpper}\\${bullet.slice(-1)}`;
        }
        else if (ROMAN_LOWER_RE.test(bullet)) {
            type = 'lower-roman';
            bull = `${romanLower}\\${bullet.slice(-1)}`;
        }
        else if (LOWER_ALPHA_RE.test(bullet)) {
            type = 'lower-alpha';
            bull = `[a-z]\\${bullet.slice(-1)}`;
        }
        else if (UPPER_ALPHA_RE.test(bullet)) {
            type = 'upper-alpha';
            bull = `[A-Z]\\${bullet.slice(-1)}`;
        }
        else {
            type = 'decimal';
            bull = `\\d{1,9}\\${bullet.slice(-1)}`;
        }
    }
    else {
        bull = options.pedantic ? escapeForRegex(bullet) : '[*+-]';
    }
    const list: ListToken | null = sourceOnly
        ? null
        : {
            type: 'list',
            raw: '',
            ordered: isOrdered,
            listType: isOrdered ? type : null,
            loose: false,
            start: undefined, // Will be set when first item is processed
            tokens: []
        };
    // Get next list item
    // Updated regex to properly handle empty list items (space after bullet, then newline)
    const itemRegex = getItemRegex(bull);
    let endsWithBlankLine = false;
    let sourceOnlyConsumed = 0;
    let sourceOnlyItems = 0;
    let sourceOnlySealedLen = 0;
    let sourceOnlyLastItemLen = 0;
    // Check if current bullet point can start a new List Item
    while (src) {
        let raw = '';
        let itemContents = '';
        let endEarly = false;
        if (!(cap = itemRegex.exec(src)))
            break;
        raw = cap[0];
        const bullet = cap[1].trim();
        src = src.substring(raw.length);
        const line = cap[2]
            ? cap[2].replace(/^\t+/, (t) => ' '.repeat(4 * t.length))
            : '';
        const nextLine = firstLineOf(src);
        const blankLine = !line.trim();
        let indent = 0;
        if (options.pedantic) {
            indent = 2;
            if (!sourceOnly)
                itemContents = line.trimStart();
        }
        else if (blankLine) {
            indent = cap[1].length + 1;
        }
        else {
            indent = cap[2].search(/[^ ]/); // Find first non-space char
            indent = indent > 4 ? 1 : indent; // Treat indented code blocks (> 4 spaces) as having only 1 indent
            if (!sourceOnly)
                itemContents = line.slice(indent);
            indent += cap[1].length;
        }
        if (blankLine && /^[ \t]*$/.test(nextLine)) {
            // Items begin with at most one blank line
            raw += nextLine + '\n';
            src = src.substring(nextLine.length + 1);
            endEarly = true;
        }
        if (!endEarly) {
            const { nextBullet, hr, fences, heading, html } = LIST_BOUNDARY_TABLE[Math.min(3, indent - 1)];
            let sawBlankLine = false;
            // Check if following lines should be included in List Item
            while (src) {
                const rawLine = firstLineOf(src);
                const nextLineWithoutTabs = rawLine.includes('\t')
                    ? rawLine.replace(/\t/g, '    ')
                    : rawLine;
                const isBlankLine = !nextLineWithoutTabs.trim();
                const isIndented = nextLineWithoutTabs.search(/[^ ]/) >= indent;
                if (fences.test(nextLineWithoutTabs) ||
                    heading.test(nextLineWithoutTabs) ||
                    html.test(nextLineWithoutTabs) ||
                    nextBullet.test(nextLineWithoutTabs) ||
                    hr.test(nextLineWithoutTabs))
                    break;
                // A blank line followed by a dedented non-bullet line ends the item:
                // that line starts a new top-level block (a bullet after a blank line
                // is a loose-list continuation and was already caught by nextBullet).
                if (sawBlankLine && !isBlankLine && !isIndented)
                    break;
                if (!sourceOnly) {
                    if (isIndented || isBlankLine) {
                        itemContents += '\n' + nextLineWithoutTabs.slice(indent);
                    }
                    else {
                        itemContents += '\n' + nextLineWithoutTabs;
                    }
                }
                sawBlankLine = isBlankLine;
                raw += rawLine + '\n';
                src = src.substring(rawLine.length + 1);
            }
        }
        if (list && !list.loose) {
            // If the previous item ended with a blank line, the list is loose
            if (endsWithBlankLine) {
                list.loose = true;
            }
            else if (/\n[ \t]*\n[ \t]*$/.test(raw)) {
                endsWithBlankLine = true;
            }
        }
        if (sourceOnly) {
            if (sourceOnlyItems > 0)
                sourceOnlySealedLen += sourceOnlyLastItemLen;
            sourceOnlyLastItemLen = raw.length;
            sourceOnlyConsumed += raw.length;
            sourceOnlyItems++;
            continue;
        }
        // `list` is built iff `sourceOnly` is false, and the source-only
        // branch above already `continue`d — this narrows it for the rest.
        if (!list)
            continue;
        let isTask: RegExpExecArray | null = null;
        let isChecked = false;
        // Check for task list items
        if (options.gfm) {
            isTask = /^\[[ xX]] /.exec(itemContents);
            if (isTask) {
                isChecked = isTask[0] !== '[ ] ';
                itemContents = itemContents.replace(/^\[[ xX]] +/, '');
            }
        }
        let value: number | null = null;
        if (!isOrdered) {
            // Do nothing for unordered lists
        }
        else if (type === 'decimal') {
            value = parseInt(bullet.slice(0, -1), 10);
        }
        else if (type === 'lower-alpha' || type === 'upper-alpha') {
            value = letterToInt(bullet.slice(0, -1));
        }
        else if (type === 'lower-roman' || type === 'upper-roman') {
            value = romanToInt(bullet.slice(0, -1));
        }
        // Handle expectedValue initialization and validation
        let skipped = false;
        if (isOrdered) {
            if (expectedValue === null) {
                // First item: set expectedValue to this item's value (or 1 if parsing failed)
                expectedValue = value ?? 1;
                // Set the start property for ordered lists
                list.start = expectedValue;
            }
            else {
                // Subsequent items: check if value matches expected
                skipped = value !== null && value !== expectedValue;
                // Increment expectedValue for next item
                expectedValue += 1;
            }
        }
        list.tokens.push({
            type: 'list_item',
            raw,
            task: !!isTask,
            checked: isChecked,
            loose: false,
            text: itemContents,
            value,
            skipped,
            tokens: []
        });
        list.raw += raw;
    }
    if (sourceOnly) {
        if (sourceOnlyItems === 0)
            return undefined;
        return {
            type: 'list',
            raw: originalSource.slice(0, sourceOnlyConsumed).trimEnd(),
            sealedLen: sourceOnlySealedLen
        };
    }
    if (!list || list.tokens.length === 0)
        return undefined;
    return list;
}
export const markedListBlock: Extension = {
    name: 'list',
    level: 'block',
    tokenizer(src) {
        const list = parseListSource(src, this.lexer.options, true);
        if (!list)
            return undefined;
        return list;
    }
};
export const markedList: Extension = {
    name: 'list',
    level: 'block',
    tokenizer(src) {
        const list = parseListSource(src, this.lexer.options);
        if (!list)
            return undefined;
        finalizeList(list, this.lexer);
        return list;
    }
};

export interface ListBoundaryToken {
    type: 'list';
    raw: string;
    sealedLen: number;
}
export interface ListToken {
    type: 'list';
    raw: string;
    ordered: boolean;
    listType: 'decimal' | 'lower-alpha' | 'upper-alpha' | 'lower-roman' | 'upper-roman' | null;
    loose: boolean;
    start?: number;
    tokens: ListItemToken[];
}
/** Token representing an item in an extended list. */
export interface ListItemToken {
    type: 'list_item';
    raw: string;
    task: boolean;
    checked: boolean;
    loose: boolean;
    text: string;
    value: number | null;
    skipped: boolean;
    tokens: GenericToken[];
}
