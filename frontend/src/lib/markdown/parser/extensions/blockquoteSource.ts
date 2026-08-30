import type { Extension } from '../index';
import { block } from '../engine';

// parseBlocks only needs the source boundary. The blockquote tokenizer
// recursively tokenizes the quote body, then parseBlocks throws that child
// tree away. Use the engine's own active GFM boundary grammar and preserve
// the exact raw prefix without paying for children.
const BLOCKQUOTE = block.gfm.blockquote;

export const parseBlockquoteSource = (src: string): string | undefined => {
    let index = 0;
    while (index < src.length && index < 4 && src.charCodeAt(index) === 32)
        index++;
    if (index > 3 || src.charCodeAt(index) !== 62)
        return undefined;
    return BLOCKQUOTE.exec(src)?.[0];
};

export const markedBlockquoteBlock: Extension = {
    name: 'blockquote',
    level: 'block',
    tokenizer(src) {
        const raw = parseBlockquoteSource(src);
        if (raw === undefined)
            return undefined;
        return { type: 'blockquote', raw };
    }
};
