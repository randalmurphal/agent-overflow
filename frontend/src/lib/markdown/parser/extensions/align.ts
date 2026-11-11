import type { Extension } from '../index';
import type { Token } from '../engine';
const CENTER_BLOCK = /^\[center\]\n(?:([\s\S]*?)\n)?(?:\[\/center\]|(?=\[(?:center|right)\]\n))/;
const RIGHT_BLOCK = /^\[right\]\n(?:([\s\S]*?)\n)?(?:\[\/right\]|(?=\[(?:center|right)\]\n))/;
export const parseAlignSource = (
    src: string
): { raw: string; text: string; align: 'center' | 'right' } | undefined => {
    if (src.charCodeAt(0) !== 91)
        return undefined;
    const match = CENTER_BLOCK.exec(src);
    if (match)
        return { raw: match[0], text: match[1] ?? '', align: 'center' as const };
    const right = RIGHT_BLOCK.exec(src);
    return right
        ? { raw: right[0], text: right[1] ?? '', align: 'right' as const }
        : undefined;
};
const OPENER = /^\[(?:center|right)\]\n/;
// Block splitting only: a block with no closer yet runs to the end of the
// source, so a streaming tail completes it as one block (the completer
// seals it) instead of splitting it at a blank line inside it. Lexing the
// unclosed block still yields what it shows today, plain blocks.
export const markedAlignBlock: Extension = {
    name: 'align',
    level: 'block',
    tokenizer(src) {
        const source = parseAlignSource(src);
        if (source)
            return { type: 'align', raw: source.raw };
        return src.charCodeAt(0) === 91 && OPENER.test(src) ? { type: 'align', raw: src } : undefined;
    }
};
export const markedAlign: Extension = {
    name: 'align',
    level: 'block',
    tokenizer(src) {
        const source = parseAlignSource(src);
        if (!source)
            return undefined;
        return {
            type: 'align',
            ...source,
            tokens: this.lexer.blockTokens(source.text, [])
        };
    }
};

export type AlignToken = {
    type: 'align';
    align: 'center' | 'right';
    raw: string;
    text: string;
    tokens: Token[];
};
