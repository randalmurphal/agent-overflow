import type { Extension } from '../index';
import type { GenericToken } from '../index';
// Hoisted to module scope: these are stateless (no `g` flag) and were previously
// recompiled on every tokenizer call (DL_RULE) and every line of a growing list (DL_LINE_RULE).
// The detail group must accept colons (`Time: 10:30`) and mirror DL_RULE exactly:
// any line the block rule consumes must also match here, or it silently vanishes.
const DL_RULE = /^(?:[ \t]*:[^:\n]+:[ \t]?[^\n]*(?:\n|$))+/;
const DL_LINE_RULE = /^\s*:([^:\n]+):([^\n]*)(?:\n|$)/;
export const descriptionListSource = (src: string): string | undefined => {
    let markerIndex = 0;
    while (src.charCodeAt(markerIndex) === 32 || src.charCodeAt(markerIndex) === 9)
        markerIndex++;
    if (src.charCodeAt(markerIndex) !== 58)
        return undefined;
    return DL_RULE.exec(src)?.[0];
};
export const markedDlBlock: Extension = {
    name: 'descriptionList',
    level: 'block',
    tokenizer(src) {
        const raw = descriptionListSource(src);
        return raw === undefined ? undefined : { type: 'descriptionList', raw };
    }
};
export const markedDl: Extension = {
    name: 'descriptionList',
    level: 'block', // Is this a block-level or inline-level tokenizer?
    tokenizer(src) {
        const raw = descriptionListSource(src);
        if (raw === undefined)
            return undefined;
        const text = raw.trim();
        const tokens: DescriptionToken[] = [];
        // Parse each line as a description
        const lines = text.split('\n');
        for (const line of lines) {
            const lineMatch = DL_LINE_RULE.exec(line);
            if (lineMatch) {
                const term = lineMatch[1].trim();
                const detail = lineMatch[2].trim();
                tokens.push({
                    type: 'description',
                    raw: lineMatch[0],
                    tokens: [
                        {
                            type: 'descriptionTerm',
                            raw: term,
                            tokens: this.lexer.inlineTokens(term)
                        },
                        {
                            type: 'descriptionDetail',
                            raw: detail,
                            tokens: this.lexer.inlineTokens(detail)
                        }
                    ]
                });
            }
        }
        return {
            type: 'descriptionList',
            raw,
            text,
            tokens
        };
    }
};

export type DescriptionListToken = {
    type: 'descriptionList';
    raw: string;
    text: string;
    tokens: DescriptionToken[];
};
export type DescriptionToken = {
    type: 'description';
    raw: string;
    tokens: [DescriptionTermToken, DescriptionDetailToken];
};
export type DescriptionTermToken = {
    type: 'descriptionTerm';
    raw: string;
    tokens: GenericToken[];
};
export type DescriptionDetailToken = {
    type: 'descriptionDetail';
    raw: string;
    tokens: GenericToken[];
};
