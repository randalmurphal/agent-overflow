/**
 * The completer contract, and the line scans its handlers share.
 *
 * `IncompleteMarkdownParser` (incompleteMarkdown.ts) runs a list of these
 * plugins over the streaming volatile tail: a `preprocess` pass may rewrite
 * the whole text and publish state, a per-line `handler` rewrites one line,
 * and a `postprocess` pass closes what is still open at the end. Plugin
 * FAMILIES live in the three sibling modules (`.context`, `.inline`,
 * `.structural`); nothing here parses anything itself.
 */
export interface Plugin {
    name: string;
    pattern?: RegExp;
    handler?: (payload: HandlerPayload) => string;
    skipInBlockTypes?: Array<keyof LineContext>;
    preprocess?: (payload: HookPayload) => string | {
        text: string;
        state: Partial<ParseState>;
    };
    postprocess?: (payload: HookPayload) => string;
}
export interface HookPayload {
    text: string;
    state: ParseState;
    setState: (state: Partial<ParseState>) => void;
}
export interface HandlerPayload {
    line: string;
    text: string;
    match: RegExpMatchArray;
    state: ParseState;
    setState: (state: Partial<ParseState>) => void;
}
/** Which blocking constructs a given source line sits inside. */
export interface LineContext {
    code: boolean;
    math: boolean;
    center: boolean;
    right: boolean;
}
/**
 * The fence awaiting its closer, as the context-manager plugin records it.
 * (The old hand-written declaration called this a `string`; nothing ever
 * type-checked it, and the plugin has always stored this object.)
 */
export interface FenceInfo {
    prefix: string;
    char: string;
    length: number;
    lineIndex: number;
}
export interface ParseState {
    currentLine: number;
    context: 'normal' | 'list' | 'blockquote' | 'descriptionList';
    blockingContexts: Set<'code' | 'math' | 'center' | 'right'>;
    lineContexts?: LineContext[];
    fenceInfo?: FenceInfo;
    mdxUnclosedTags?: Array<{
        tagName: string;
        lineIndex: number;
    }>;
    mdxLineStates?: Array<{
        inMdx: boolean;
        incompletePositions: number[];
    }>;
}

// --- Line scans shared by the completer families ---------------------------
export const findEndOfCellOrLineContaining = (text: string, position: number): number => {
    let endPos = position;
    while (endPos < text.length && text[endPos] !== '\n' && text[endPos] !== '|') {
        endPos++;
    }
    return endPos;
};
export const isWithinMathBlock = (text: string, position: number): boolean => {
    let inInlineMath = false;
    let inBlockMath = false;
    for (let i = 0; i < text.length && i < position; i++) {
        if (text[i] === '\\' && text[i + 1] === '$') {
            i++;
            continue;
        }
        if (text[i] === '$') {
            if (text[i + 1] === '$') {
                inBlockMath = !inBlockMath;
                i++;
                inInlineMath = false;
            }
            else if (!inBlockMath) {
                inInlineMath = !inInlineMath;
            }
        }
    }
    return inInlineMath || inBlockMath;
};
export const isWithinFootnoteRef = (text: string, position: number): boolean => {
    let openBracketPos = -1;
    let caretPos = -1;
    for (let i = position; i >= 0; i--) {
        if (text[i] === ']')
            return false;
        if (text[i] === '^' && caretPos === -1)
            caretPos = i;
        if (text[i] === '[') {
            openBracketPos = i;
            break;
        }
    }
    if (openBracketPos !== -1 && caretPos === openBracketPos + 1 && position >= caretPos) {
        for (let i = position + 1; i < text.length; i++) {
            if (text[i] === ']')
                return true;
            if (text[i] === '[' || text[i] === '\n')
                break;
        }
    }
    return false;
};
