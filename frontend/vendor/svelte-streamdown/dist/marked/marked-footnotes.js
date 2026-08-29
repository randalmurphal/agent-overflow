import {} from './index.js';
import { StreamdownContext } from '../context.svelte.js';
import { getContext } from 'svelte';
const footnoteRegex = /^\[\^([^\]\n]+)\]:(?:[ \t]+|\n|$)([^\n]*(?:\n(?:[ \t]+[^\n]*)?)*)/;
const footnoteRefRegex = /^\[\^([^\]\n]+)\]/;
const footNoteLastLineRegex = /^[ \t]*?[>\-*][ ]|[`]{3,}$|^[ \t]*?[|].+[|]$/;
const safeGetContext = () => {
    try {
        return getContext('streamdown');
    }
    catch (e) {
        return null;
    }
};
export const markedFootnoteBlock = {
    name: 'footnote',
    level: 'block',
    tokenizer(src) {
        if (src.charCodeAt(0) !== 91 || src.charCodeAt(1) !== 94)
            return undefined;
        const match = footnoteRegex.exec(src);
        return match ? { type: 'footnote', raw: match[0] } : undefined;
    }
};
export function markedFootnote() {
    const ensureMaps = (tokenizer) => {
        const streamdown = safeGetContext();
        if (!streamdown) {
            if (!tokenizer.lexer.hasFootnotes) {
                tokenizer.lexer.footnotes = {
                    refs: new Map(),
                    footnotes: new Map()
                };
                tokenizer.lexer.hasFootnotes = true;
            }
            return tokenizer.lexer.footnotes;
        }
        else {
            return streamdown.footnotes;
        }
    };
    return [
        {
            name: 'footnote',
            level: 'block',
            tokenizer(src) {
                if (src.charCodeAt(0) !== 91 || src.charCodeAt(1) !== 94)
                    return undefined;
                const maps = ensureMaps(this);
                const match = footnoteRegex.exec(src);
                if (match) {
                    const [raw, label, text = ''] = match;
                    let content = text.split('\n').reduce((acc, curr) => {
                        return acc + '\n' + curr.replace(/^[ \t]+/, '');
                    }, '');
                    const contentLastLine = content.trimEnd().split('\n').pop();
                    content +=
                        // add lines after list, blockquote, codefence, and table
                        contentLastLine && footNoteLastLineRegex.test(contentLastLine) ? '\n\n' : '';
                    const lines = content.split('\n');
                    const token = {
                        type: 'footnote',
                        raw,
                        label,
                        lines,
                        tokens: []
                    };
                    maps.footnotes.set(label, token);
                    const ref = maps.refs.get(label);
                    if (ref) {
                        ref.content = token;
                    }
                    return token;
                }
            }
        },
        {
            name: 'footnoteRef',
            level: 'inline',
            tokenizer(src) {
                if (src.charCodeAt(0) !== 91 || src.charCodeAt(1) !== 94)
                    return undefined;
                const maps = ensureMaps(this);
                const match = footnoteRefRegex.exec(src);
                if (match) {
                    const [raw, label] = match;
                    const footnote = maps.footnotes.get(label);
                    const token = {
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
                    maps.refs.set(label, token);
                    return token;
                }
            }
        }
    ];
}
