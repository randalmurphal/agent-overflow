import type { Extension } from '../index';
import type { Lexer } from '../engine';

/**
 * `[center]` / `[right]` alignment, as markers between top-level blocks.
 *
 * A line that is exactly `[center]` or `[right]`, ended by a newline, at a
 * top-level block start opens alignment; the line `[/center]` or
 * `[/right]` closes it when it is the alignment in force, at a block start
 * or ending a top-level paragraph or list. Every top-level block between
 * them renders aligned, each one on its own, so a completed block streams,
 * commits and renders like any other block. An opener with no closer aligns
 * to the end of the message, streaming and settled alike; a closer of an
 * alignment not in force is text. Nothing inside code, quotes, list items,
 * tables or HTML is a marker. Until its newline arrives an opener line may
 * still become text, so a streaming opener shows as text until then.
 *
 * `alignmentMarker` is the one recognizer: the lexer calls it, and every
 * consumer of block strings folds alignment with `isAlignmentMarkerBlock`
 * and `alignmentAfter`. A block string is lexed alone, without the
 * alignment before it, so a consumer renders no marker block at all.
 */
export type Alignment = 'center' | 'right' | null;

export type AlignToken = {
    type: 'align';
    raw: string;
    align: 'center' | 'right';
    /** Opens the alignment; false for its closer. */
    open: boolean;
};

// An opener needs its newline; a closer in force may end the source.
const MARKER = /^\[(?:(center|right)\]\r?\n|\/(center|right)\](?:\r?\n|$))/;

let enabled = true;
/** Test seam: compare the pipeline with alignment off. */
export const __setAlignmentEnabledForTest = (value: boolean): void => {
    enabled = value;
};

/**
 * Runs `lex` with no markers recognized: for a block string lexed alone,
 * whose markers parseBlocks already split off. A streaming block is lexed
 * trimmed, so an indented `  [center]` line would otherwise start it.
 */
export const withoutAlignmentMarkers = <T>(lex: () => T): T => {
    const saved = enabled;
    enabled = false;
    try {
        return lex();
    } finally {
        enabled = saved;
    }
};

/** The marker `src` starts with, with `current` in force, if any. */
export const alignmentMarker = (
    src: string,
    current: Alignment
): { raw: string; align: 'center' | 'right'; open: boolean } | undefined => {
    if (!enabled || src.charCodeAt(0) !== 91)
        return undefined;
    const match = MARKER.exec(src);
    if (!match)
        return undefined;
    const open = match[1] !== undefined;
    const align = (match[1] ?? match[2]) as 'center' | 'right';
    if (!open && align !== current)
        return undefined;
    return { raw: match[0], align, open };
};

const markerOfBlock = (block: string, before: Alignment) => {
    const marker = alignmentMarker(block, before);
    if (!marker)
        return undefined;
    // A block string carries the blank lines after its token.
    for (let at = marker.raw.length; at < block.length; at++) {
        const code = block.charCodeAt(at);
        if (code !== 10 && code !== 13 && code !== 32 && code !== 9)
            return undefined;
    }
    return marker;
};

/** Whether the top-level block string `block` is a marker, with `before` in force. */
export const isAlignmentMarkerBlock = (block: string, before: Alignment): boolean =>
    markerOfBlock(block, before) !== undefined;

/** The alignment in force after the top-level block string `block`. */
export const alignmentAfter = (block: string, before: Alignment): Alignment => {
    const marker = markerOfBlock(block, before);
    if (!marker)
        return before;
    return marker.open ? marker.align : null;
};

/** The closer line that ends the alignment in force, for a top-level
 * construct that would otherwise continue over it. */
export const openAlignmentCloser = (lexer: Lexer): string | undefined =>
    enabled && lexer.align !== null && lexer.blockDepth === 1 ? `[/${lexer.align}]` : undefined;

/** Whether `line` (no newline) is the closer `closer`. */
export const isAlignmentCloserLine = (line: string, closer: string): boolean =>
    line === closer || (line.length === closer.length + 1 && line.endsWith('\r') && line.startsWith(closer));

const CLOSER_LINE_MAX = '[/center]\r'.length;

/** The last line of a source, while short enough to be a closer, else null. */
export const shortLastLine = (source: string): string | null =>
    nextShortLastLine('', source);

/** `shortLastLine` of a source after `delta` is appended, from the
 * previous result: a growing source is never scanned. */
export const nextShortLastLine = (line: string | null, delta: string): string | null => {
    const newline = delta.lastIndexOf('\n');
    const next = newline >= 0 ? delta.slice(newline + 1) : line === null ? null : line + delta;
    return next !== null && next.length <= CLOSER_LINE_MAX ? next : null;
};

/**
 * `block` without its last line `line` (from `shortLastLine`) when that
 * line may still become the closer of `align`: shown, it would vanish when
 * the closer completes. A closer with no newline yet is held too, since a
 * same-line append may reach a block before parseBlocks splits it off.
 */
export const withoutPartialAlignmentCloser = (
    block: string,
    align: 'center' | 'right',
    line: string | null
): string => {
    if (line === null || line === '')
        return block;
    const closer = `[/${align}]`;
    const partial = line.length <= closer.length ? closer.startsWith(line) : line === `${closer}\r`;
    return partial ? block.slice(0, block.length - line.length) : block;
};

// Top-level tokens with no element, which get no aligned wrapper either.
const rendersNothing = (type: string, renderHtml: boolean): boolean =>
    type === 'space' || type === 'def' || type === 'footnote' || (type === 'html' && !renderHtml);

/**
 * The top-level tokens to render with the alignment each renders under,
 * markers removed; null when no alignment applies, so the caller renders
 * the tokens unchanged.
 */
export const alignedTokens = <T extends { type: string }>(
    tokens: T[],
    align: Alignment,
    renderHtml: boolean
): { token: T; align: Alignment }[] | null => {
    if (align === null && !tokens.some((token) => token.type === 'align'))
        return null;
    const out: { token: T; align: Alignment }[] = [];
    let current = align;
    for (const token of tokens) {
        if (token.type === 'align') {
            const marker = token as unknown as AlignToken;
            current = marker.open ? marker.align : null;
        } else
            out.push({ token, align: rendersNothing(token.type, renderHtml) ? null : current });
    }
    return out;
};

export const markedAlign: Extension = {
    name: 'align',
    level: 'block',
    // A top-level paragraph ends before the closer of the alignment in force.
    // The paragraph cannot pass a blank line, so the scan stops at one.
    start(src) {
        const closer = openAlignmentCloser(this.lexer);
        if (closer === undefined)
            return undefined;
        for (let at = src.indexOf('\n'); at >= 0; at = src.indexOf('\n', at + 1)) {
            const end = src.indexOf('\n', at + 1);
            const line = src.slice(at + 1, end < 0 ? src.length : end);
            if (line.trim() === '')
                return undefined;
            if (isAlignmentCloserLine(line, closer))
                return at + 1;
        }
        return undefined;
    },
    tokenizer(src) {
        if (this.lexer.blockDepth !== 1)
            return undefined;
        const marker = alignmentMarker(src, this.lexer.align);
        if (!marker)
            return undefined;
        this.lexer.align = marker.open ? marker.align : null;
        return { type: 'align', raw: marker.raw, align: marker.align, open: marker.open };
    }
};
