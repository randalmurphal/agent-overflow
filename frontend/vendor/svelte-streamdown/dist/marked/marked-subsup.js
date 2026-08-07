const subRule = /^~([^~`\s]+)~(?!\d)/; // ~text~ — no spaces/backticks (code spans bind tighter); closing ~ not before a digit so approx-range prose like ~5~10 / ~50~100 stays plain instead of sub(5)
const supRule = /^\^([^\^`\s](?:[^\^`]*[^\^`\s])?)\^/; // ^text^ — no backticks (code spans bind tighter)
export const markedSub = {
    name: 'sub',
    level: 'inline',
    start(src) {
        const i = src.indexOf('~');
        return i === -1 ? undefined : i;
    },
    tokenizer(src) {
        const match = src.match(subRule);
        if (match) {
            return {
                type: 'sub',
                raw: match[0],
                text: match[1],
                tokens: this.lexer.inlineTokens(match[1])
            };
        }
    }
};
export const markedSup = {
    name: 'sup',
    level: 'inline',
    start(src) {
        const i = src.indexOf('^');
        return i === -1 ? undefined : i;
    },
    tokenizer(src) {
        const match = src.match(supRule);
        if (match) {
            return {
                type: 'sup',
                raw: match[0],
                text: match[1],
                tokens: this.lexer.inlineTokens(match[1])
            };
        }
    }
};
