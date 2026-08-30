import type { Extension, StreamdownToken } from '../index';
import type { StreamdownContext } from '../../render/context.svelte';
import type { TokenizerThis } from '../engine';
import { getContext } from 'svelte';
const footnoteRegex = /^\[\^([^\]\n]+)\]:(?:[ \t]+|\n|$)([^\n]*(?:\n(?:[ \t]+[^\n]*)?)*)/;
const footnoteRefRegex = /^\[\^([^\]\n]+)\]/;
const footNoteLastLineRegex = /^[ \t]*?[>\-*][ ]|[`]{3,}$|^[ \t]*?[|].+[|]$/;
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
    tokenizer(src) {
        if (src.charCodeAt(0) !== 91 || src.charCodeAt(1) !== 94)
            return undefined;
        const match = footnoteRegex.exec(src);
        return match ? { type: 'footnote', raw: match[0] } : undefined;
    }
};
export function markedFootnote(): Extension[] {
    const ensureMaps = (tokenizer: TokenizerThis): FootnoteMaps => {
        const streamdown = safeGetContext();
        if (!streamdown) {
            const lexer = tokenizer.lexer;
            lexer.footnotes ??= {
                refs: new Map(),
                footnotes: new Map()
            };
            return lexer.footnotes;
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
                    const token: Footnote = {
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
                    maps.refs.set(label, token);
                    return token;
                }
            }
        }
    ];
}

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
