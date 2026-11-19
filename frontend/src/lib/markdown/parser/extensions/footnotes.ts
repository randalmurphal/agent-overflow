import type { Extension, StreamdownToken } from '../index';
import type { StreamdownContext } from '../../render/context.svelte';
import type { Lexer, TokenizerThis } from '../engine';
import { readDocument } from '../documentScope';
import { getContext } from 'svelte';
const footnoteStartRegex = /^\[\^([^\]\n]+)\]:(?:[ \t]+|(?=\n)|$)/;
const footnoteRefRegex = /^\[\^([^\]\n]+)\]/;
// A line that starts another block, so it cannot continue a definition's
// paragraph lazily.
const interruptsRegex = /^ {0,3}(?:\[\^[^\]\n]+\]:|#{1,6}(?:[ \t]|$)|`{3,}|~{3,}|>|[-*+](?:[ \t]|$)|\d{1,9}[.)](?:[ \t]|$)|(?:\*[ \t]*){3,}$|(?:-[ \t]*){3,}$|(?:_[ \t]*){3,}$|<[A-Za-z/!?])/;
const fenceRegex = /^ {0,3}(`{3,}|~{3,})/;
/** Columns of indentation and the offset after them, with tabs to the next stop of 4. */
const indentOf = (line: string): { columns: number; offset: number } => {
    let columns = 0;
    let offset = 0;
    while (offset < line.length) {
        const code = line.charCodeAt(offset);
        if (code === 32) columns++;
        else if (code === 9) columns += 4 - (columns % 4);
        else break;
        offset++;
    }
    return { columns, offset };
};
/** `line` with up to 4 columns of indentation removed. */
const dedent = (line: string): string => {
    let columns = 0;
    let offset = 0;
    while (offset < line.length && columns < 4) {
        const code = line.charCodeAt(offset);
        if (code === 32) columns++;
        else if (code === 9) {
            const width = 4 - (columns % 4);
            if (columns + width > 4) return ' '.repeat(columns + width - 4) + line.slice(offset + 1);
            columns += width;
        }
        else break;
        offset++;
    }
    return line.slice(offset);
};
/**
 * A footnote definition at the start of `src`, as GFM reads one: the
 * container holds the first line, every later line indented four columns
 * (blank lines between them included), and lines continuing its open
 * paragraph lazily. `lines` is the body with the container indentation
 * removed; `raw` ends at its last line's newline.
 */
const scanFootnoteDefinition = (src: string): { raw: string; label: string; lines: string[] } | undefined => {
    const start = footnoteStartRegex.exec(src);
    if (!start) return undefined;
    const label = start[1];
    let lineEnd = src.indexOf('\n', start[0].length);
    if (lineEnd === -1) lineEnd = src.length;
    const first = src.slice(start[0].length, lineEnd);
    const lines = [first];
    let fence: string | null = null;
    const track = (content: string): boolean => {
        const open = fenceRegex.exec(content);
        if (fence !== null) {
            if (open && open[1][0] === fence[0] && open[1].length >= fence.length && content.trim() === open[1]) fence = null;
            return false;
        }
        if (open) {
            fence = open[1];
            return false;
        }
        return content.trim() !== '' && !/^ {0,3}#{1,6}(?:[ \t]|$)/.test(content);
    };
    let paragraph = track(first);
    let end = lineEnd < src.length ? lineEnd + 1 : lineEnd;
    let pos = end;
    let blanks: string[] = [];
    while (pos < src.length) {
        let next = src.indexOf('\n', pos);
        if (next === -1) next = src.length;
        const line = src.slice(pos, next);
        const after = next < src.length ? next + 1 : next;
        if (line.trim() === '') {
            blanks.push('');
            pos = after;
            continue;
        }
        if (indentOf(line).columns >= 4) {
            lines.push(...blanks, dedent(line));
            blanks = [];
            paragraph = track(dedent(line));
        }
        else if (blanks.length === 0 && paragraph && fence === null && !interruptsRegex.test(line)) {
            lines.push(line);
        }
        else {
            break;
        }
        end = after;
        pos = after;
    }
    return { raw: src.slice(0, end), label, lines };
};
/**
 * Where a definition line interrupts the paragraph at the start of `src`,
 * for the block loop's paragraph clipping; a blank line ends the search,
 * since it ends the paragraph.
 */
const footnoteStart = (src: string): number | undefined => {
    let at = src.indexOf('\n');
    while (at !== -1 && at + 1 < src.length) {
        const next = at + 1;
        if (src.charCodeAt(next) === 10) return undefined;
        if (src.charCodeAt(next) === 91 && src.charCodeAt(next + 1) === 94 && footnoteStartRegex.test(src.slice(next)))
            return next;
        at = src.indexOf('\n', next);
    }
    return undefined;
};
const safeGetContext = (): StreamdownContext | null => {
    try {
        return getContext<StreamdownContext | undefined>('streamdown') ?? null;
    }
    catch (e) {
        return null;
    }
};
export const markedFootnoteBlock: Extension = {
    name: 'footnote',
    level: 'block',
    start: footnoteStart,
    tokenizer(src) {
        if (src.charCodeAt(0) !== 91 || src.charCodeAt(1) !== 94)
            return undefined;
        const definition = scanFootnoteDefinition(src);
        return definition ? { type: 'footnote', raw: definition.raw } : undefined;
    }
};
// How many references to each label a forge-numbered lex has met.
const referenceCounts = new WeakMap<Lexer, Map<string, number>>();
export function markedFootnote(): Extension[] {
    const ensureMaps = (tokenizer: TokenizerThis): FootnoteMaps => {
        // A pre-seeded Lexer owns its maps unconditionally.
        // `lexFootnoteDefinitions` and `lexCapture` seed `lexer.footnotes`
        // BEFORE the run precisely so a whole-document lex stays isolated
        // from any ambient Streamdown context it happens to execute under:
        // a context hit here would write this document's definitions into
        // that surface's maps and leave the caller's lexer empty.
        const lexer = tokenizer.lexer;
        if (lexer.footnotes)
            return lexer.footnotes;
        const streamdown = safeGetContext();
        if (!streamdown) {
            lexer.footnotes = {
                refs: new Map(),
                footnotes: new Map()
            };
            return lexer.footnotes;
        }
        return streamdown.footnotes;
    };
    return [
        {
            name: 'footnote',
            level: 'block',
            start: footnoteStart,
            tokenizer(src) {
                if (src.charCodeAt(0) !== 91 || src.charCodeAt(1) !== 94)
                    return undefined;
                const definition = scanFootnoteDefinition(src);
                if (!definition)
                    return undefined;
                const maps = ensureMaps(this);
                const token: Footnote = {
                    type: 'footnote',
                    raw: definition.raw,
                    label: definition.label,
                    lines: definition.lines,
                    tokens: []
                };
                maps.footnotes.set(definition.label, token);
                const ref = maps.refs.get(definition.label);
                if (ref) {
                    ref.content = token;
                }
                return token;
            }
        },
        {
            name: 'footnoteRef',
            level: 'inline',
            tokenizer(src) {
                if (src.charCodeAt(0) !== 91 || src.charCodeAt(1) !== 94)
                    return undefined;
                const match = footnoteRefRegex.exec(src);
                if (!match)
                    return undefined;
                const [raw, label] = match;
                // A forge numbers each defined label by its first reference
                // and leaves a reference to an undefined one as its text.
                const lexer = this.lexer;
                const numbers = lexer.document?.footnotes;
                let number: number | undefined;
                let anchor: string | undefined;
                if (numbers) {
                    readDocument(lexer, `f:${label}`);
                    number = numbers.get(label);
                    if (number === undefined)
                        return undefined;
                    let counts = referenceCounts.get(lexer);
                    if (!counts)
                        referenceCounts.set(lexer, (counts = new Map()));
                    const local = (counts.get(label) ?? 0) + 1;
                    counts.set(label, local);
                    anchor = footnoteRefAnchor(number, (lexer.document?.footnoteBase?.get(label) ?? 0) + local);
                }
                const maps = ensureMaps(this);
                const footnote = maps.footnotes.get(label);
                const token: FootnoteRef = {
                    type: 'footnoteRef',
                    raw,
                    label,
                    content: footnote || {
                        type: 'footnote',
                        raw,
                        label,
                        lines: [],
                        tokens: []
                    }
                };
                if (number !== undefined) {
                    token.number = number;
                    token.anchor = anchor;
                }
                maps.refs.set(label, token);
                return token;
            }
        }
    ];
}

/** The anchor of the `occurrence`th reference (from 1) to footnote `number`.
 * A colon, which no heading slug keeps, so the two never collide. */
export const footnoteRefAnchor = (number: number, occurrence: number): string =>
    occurrence === 1 ? `fnref:${number}` : `fnref:${number}:${occurrence}`;

/** Represents a single footnote. */
export type Footnote = {
    type: 'footnote';
    raw: string;
    label: string;
    tokens: StreamdownToken[];
    lines: string[];
};
/** Represents a reference to a footnote. */
export type FootnoteRef = {
    type: 'footnoteRef';
    raw: string;
    label: string;
    content: Footnote;
    /** Its number on a forge surface (documentScope.ts). */
    number?: number;
    /** Where the footnote's back-reference returns to. */
    anchor?: string;
};
export type FootnoteToken = Footnote | FootnoteRef;
/**
 * The per-document footnote tables. Owned by the Streamdown context when one
 * exists (component rendering) and by `Lexer.footnotes` otherwise (a bare
 * `lex` / `lexFootnoteDefinitions` call) — see `ensureMaps` above.
 */
export type FootnoteMaps = {
    refs: Map<string, FootnoteRef>;
    footnotes: Map<string, Footnote>;
};
