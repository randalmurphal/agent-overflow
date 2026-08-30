const CENTER_BLOCK = /^\[center\]\n(?:([\s\S]*?)\n)?(?:\[\/center\]|(?=\[(?:center|right)\]\n))/;
const RIGHT_BLOCK = /^\[right\]\n(?:([\s\S]*?)\n)?(?:\[\/right\]|(?=\[(?:center|right)\]\n))/;
export const parseAlignSource = (src) => {
    if (src.charCodeAt(0) !== 91)
        return undefined;
    const match = CENTER_BLOCK.exec(src);
    if (match)
        return { raw: match[0], text: match[1] ?? '', align: 'center' };
    const right = RIGHT_BLOCK.exec(src);
    return right
        ? { raw: right[0], text: right[1] ?? '', align: 'right' }
        : undefined;
};
export const markedAlignBlock = {
    name: 'align',
    level: 'block',
    tokenizer(src) {
        const source = parseAlignSource(src);
        return source ? { type: 'align', raw: source.raw } : undefined;
    }
};
export const markedAlign = {
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
