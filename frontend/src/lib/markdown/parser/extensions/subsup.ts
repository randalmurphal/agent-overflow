import type { Token } from '../engine';
import type { Extension } from '../index';
const subRule = /^~([^~`\s]+)~(?!\d)/; // ~text~ — no spaces/backticks (code spans bind tighter); closing ~ not before a digit so approx-range prose like ~5~10 / ~50~100 stays plain instead of sub(5)
const supRule = /^\^([^\^`\s](?:[^\^`]*[^\^`\s])?)\^/; // ^text^ — no backticks (code spans bind tighter)
export const markedSub: Extension = {
    name: 'sub',
    level: 'inline',
    start(src) {
        const i = src.indexOf('~');
        return i === -1 ? undefined : i;
    },
    tokenizer(src) {
        if (src.charCodeAt(0) !== 126)
            return undefined;
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
export const markedSup: Extension = {
    name: 'sup',
    level: 'inline',
    start(src) {
        const i = src.indexOf('^');
        return i === -1 ? undefined : i;
    },
    tokenizer(src) {
        if (src.charCodeAt(0) !== 94)
            return undefined;
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

/** Represents a subscript token. */
export type SubToken = {
    type: 'sub';
    raw: string;
    text: string;
    tokens: Token[];
};
/** Represents a superscript token. */
export type SupToken = {
    type: 'sup';
    raw: string;
    text: string;
    tokens: Token[];
};
export type SubSupToken = SubToken | SupToken;
