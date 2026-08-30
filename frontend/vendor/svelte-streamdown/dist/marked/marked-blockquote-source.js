import { Lexer } from 'marked';

// parseBlocks only needs the source boundary. Marked's normal blockquote
// tokenizer recursively tokenizes the quote body, then parseBlocks throws that
// child tree away. Use marked's own active GFM boundary grammar and preserve
// the exact raw prefix without paying for children.
const BLOCKQUOTE = Lexer.rules.block.gfm.blockquote;

export const parseBlockquoteSource = (src) => {
    let index = 0;
    while (index < src.length && index < 4 && src.charCodeAt(index) === 32)
        index++;
    if (index > 3 || src.charCodeAt(index) !== 62)
        return undefined;
    return BLOCKQUOTE.exec(src)?.[0];
};

export const markedBlockquoteBlock = {
    name: 'blockquote',
    level: 'block',
    tokenizer(src) {
        const raw = parseBlockquoteSource(src);
        if (raw === undefined)
            return undefined;
        return { type: 'blockquote', raw };
    }
};
