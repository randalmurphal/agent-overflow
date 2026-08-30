import type { Extension } from '../index';
import type { Token } from '../engine';

/** Attribute values an MDX tag can carry: a quoted string, or a `{}` expression narrowed to a JSON scalar. */
export type MdxAttributes = Record<string, string | number | boolean>;
const SELF_CLOSING_MDX = /^<([A-Z][a-zA-Z0-9]*)((?:\s+\w+=(?:"[^"]*"|{[^}]*}))*)\s*\/>/;
const OPEN_MDX = /^<([A-Z][a-zA-Z0-9]*)((?:\s+\w+=(?:"[^"]*"|{[^}]*}))*)\s*>/;

const findTagEnd = (src: string, start: number): number => {
    let quote = 0;
    let braceDepth = 0;
    for (let index = start; index < src.length; index++) {
        const code = src.charCodeAt(index);
        if (quote !== 0) {
            if (code === quote && src.charCodeAt(index - 1) !== 92)
                quote = 0;
            continue;
        }
        if (code === 34 || code === 39) {
            quote = code;
            continue;
        }
        if (code === 123) {
            braceDepth++;
            continue;
        }
        if (code === 125 && braceDepth > 0) {
            braceDepth--;
            continue;
        }
        if (code === 62 && braceDepth === 0)
            return index;
    }
    return -1;
};

const tagIsSelfClosing = (src: string, end: number): boolean => {
    let index = end - 1;
    while (index >= 0 && (src.charCodeAt(index) === 32 || src.charCodeAt(index) === 9))
        index--;
    return src.charCodeAt(index) === 47;
};

function parseAttributes(attributeString: string): MdxAttributes {
    const attributes: MdxAttributes = {};
    const attrPattern = /(\w+)=(?:"([^"]*)"|{([^}]*)})/g;
    let match: RegExpExecArray | null;
    while ((match = attrPattern.exec(attributeString)) !== null) {
        const [, name, stringValue, expressionValue] = match;
        if (stringValue !== undefined) {
            attributes[name] = stringValue;
        }
        else if (expressionValue !== undefined) {
            const trimmed = expressionValue.trim();
            if (/^-?\d+(\.\d+)?$/.test(trimmed)) {
                attributes[name] = parseFloat(trimmed);
            }
            else if (trimmed === 'true') {
                attributes[name] = true;
            }
            else if (trimmed === 'false') {
                attributes[name] = false;
            }
            else {
                attributes[name] = trimmed;
            }
        }
    }
    return attributes;
}

/** Match one complete MDX component without building attributes or children. */
export const parseMdxSource = (
    src: string
): { raw: string; tagName: string; attributeString: string; selfClosing: boolean; content: string } | undefined => {
    const tagInitial = src.charCodeAt(1);
    if (src.charCodeAt(0) !== 60 || tagInitial < 65 || tagInitial > 90)
        return undefined;
    const selfClosingMatch = SELF_CLOSING_MDX.exec(src);
    if (selfClosingMatch) {
        const [raw, tagName, attributeString] = selfClosingMatch;
        return { raw, tagName, attributeString, selfClosing: true, content: '' };
    }
    const openTagMatch = OPEN_MDX.exec(src);
    if (!openTagMatch)
        return undefined;
    const [openTag, tagName, attributeString] = openTagMatch;
    const closingTag = `</${tagName}>`;
    let depth = 1;
    let searchPos = openTag.length;
    let closingIndex = -1;
    while (depth > 0 && searchPos < src.length) {
        const nextTag = src.indexOf('<', searchPos);
        if (nextTag === -1)
            break;
        if (src.startsWith(closingTag, nextTag)) {
            depth--;
            if (depth === 0)
                closingIndex = nextTag;
            searchPos = nextTag + closingTag.length;
            continue;
        }
        if (src.startsWith(`<${tagName}`, nextTag)) {
            const boundary = src.charCodeAt(nextTag + tagName.length + 1);
            if (boundary === 32 || boundary === 9 || boundary === 10 || boundary === 13 ||
                boundary === 47 || boundary === 62) {
                const tagEnd = findTagEnd(src, nextTag + tagName.length + 1);
                if (tagEnd === -1)
                    break;
                if (!tagIsSelfClosing(src, tagEnd))
                    depth++;
                searchPos = tagEnd + 1;
                continue;
            }
        }
        searchPos = nextTag + 1;
    }
    if (closingIndex === -1)
        return undefined;
    return {
        raw: src.substring(0, closingIndex + closingTag.length),
        tagName,
        attributeString,
        selfClosing: false,
        content: src.substring(openTag.length, closingIndex)
    };
};

export const markedMdxBlock: Extension = {
    name: 'mdx',
    level: 'block',
    applyInBlockParsing: true,
    tokenizer(src) {
        const source = parseMdxSource(src);
        return source ? { type: 'mdx', raw: source.raw } : undefined;
    }
};

export const markedMdx: Extension = {
    name: 'mdx',
    level: 'block',
    applyInBlockParsing: true,
    tokenizer(src) {
        const source = parseMdxSource(src);
        if (!source)
            return undefined;
        const attributes = parseAttributes(source.attributeString);
        if (source.selfClosing) {
            return {
                type: 'mdx',
                raw: source.raw,
                tagName: source.tagName,
                attributes,
                selfClosing: true
            };
        }
        return {
            type: 'mdx',
            raw: source.raw,
            tagName: source.tagName,
            attributes,
            selfClosing: false,
            tokens: source.content.trim()
                ? this.lexer.blockTokens(source.content.trim(), [])
                : [],
            text: source.content
        };
    }
};

export type MdxToken = {
    type: 'mdx';
    raw: string;
    tagName: string;
    attributes: MdxAttributes;
    selfClosing: boolean;
    tokens?: Token[];
    text?: string;
};
