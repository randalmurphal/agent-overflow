/**
 * `incrementalLex`' per-block cache: what a streamed Block carries between
 * ticks, and the trim machinery that derives completion-mode bounds from a
 * proven append instead of flattening the whole block through `String#trim`.
 *
 * One cache per Block component instance. `incrementalLex.ts` is its only
 * writer; `lastPath` is the test breadcrumb that proves a fast path engaged.
 */
import type { ProvenAppend } from './provenAppend';
import type { Extension, LinkTable, StreamdownToken } from './lexer';
import type { FootnoteMaps } from './extensions/footnotes';
const isTrimWhitespaceCode = (code: number): boolean => (code >= 9 && code <= 13) ||
    code === 32 ||
    code === 160 ||
    code === 5760 ||
    (code >= 8192 && code <= 8202) ||
    code === 8232 ||
    code === 8233 ||
    code === 8239 ||
    code === 8287 ||
    code === 12288 ||
    code === 65279;
export const trimBlock = (
    block: string,
    cache: IncrementalLexCache,
    complete: ((markdown: string) => string) | null,
    appendIsProven: boolean,
    appendDelta: string | undefined
): TrimmedBlock => {
    if (!complete)
        return { value: block, leading: 0, trailing: '', append: appendIsProven ? appendDelta ?? null : null };
    if (appendIsProven && cache.completeKey === complete && appendDelta) {
        let deltaEnd = appendDelta.length;
        while (deltaEnd > 0 && isTrimWhitespaceCode(appendDelta.charCodeAt(deltaEnd - 1)))
            deltaEnd--;
        if (cache.src.length === 0) {
            let deltaStart = 0;
            while (deltaStart < deltaEnd && isTrimWhitespaceCode(appendDelta.charCodeAt(deltaStart)))
                deltaStart++;
            if (deltaStart === deltaEnd)
                return { value: '', leading: block.length, trailing: '', append: '' };
            const leading = cache.input.length + deltaStart;
            const trailing = appendDelta.slice(deltaEnd);
            const append = appendDelta.slice(deltaStart, deltaEnd);
            return {
                value: leading === 0 && trailing.length === 0
                    ? block
                    : block.slice(leading, block.length - trailing.length),
                leading,
                trailing,
                append
            };
        }
        const leading = cache.leadingTrim;
        if (deltaEnd === 0) {
            return {
                value: cache.src,
                leading,
                trailing: cache.trailingTrim + appendDelta,
                append: ''
            };
        }
        const trailing = appendDelta.slice(deltaEnd);
        const append = cache.trailingTrim + appendDelta.slice(0, deltaEnd);
        return {
            value: leading === 0 && trailing.length === 0
                ? block
                : block.slice(leading, block.length - trailing.length),
            leading,
            trailing,
            append
        };
    }
    let leading = 0;
    while (leading < block.length && isTrimWhitespaceCode(block.charCodeAt(leading)))
        leading++;
    let end = block.length;
    while (end > leading && isTrimWhitespaceCode(block.charCodeAt(end - 1)))
        end--;
    const trailing = block.slice(end);
    return {
        value: leading === 0 && trailing.length === 0 ? block : block.slice(leading, end),
        leading,
        trailing,
        append: null
    };
};
export const commitLexSource = (cache: IncrementalLexCache, block: string, trim: TrimmedBlock): void => {
    cache.src = trim.value;
    cache.input = block;
    cache.leadingTrim = trim.leading;
    cache.trailingTrim = trim.trailing;
};
export const createIncrementalLexCache = (observeLex?: IncrementalLexObserver): IncrementalLexCache => ({
    src: '',
    input: '',
    extKey: null,
    completeKey: null,
    tokens: null,
    links: null,
    footnotes: null,
    codeFence: null,
    tableTailUnstable: false,
    tableAppend: null,
    leadingTrim: 0,
    trailingTrim: '',
    lastCodeTextAppend: undefined,
    lastPath: 'none',
    observeLex
});

/** The trim bounds `incrementalLex` derives instead of calling String#trim. */
export type TrimmedBlock = {
	value: string;
	leading: number;
	trailing: string;
	append: string | null;
};

/**
 * Opaque incremental state for `incrementalLex`. Create one per streamed
 * block (a Block component instance) and pass it on every call. `lastPath`
 * is a debug breadcrumb ('none' | 'full' | 'list-append' | 'table-append')
 * so tests can assert the fast path actually engaged. `links` and
 * `footnotes` carry the last full lex's reference-link table and footnote
 * maps into tail-only re-lexes.
 */
export type IncrementalLexCache = {
    src: string;
    input: string;
    extKey: Extension[] | null;
    completeKey: ((markdown: string) => string) | null;
    tokens: StreamdownToken[] | null;
    links: LinkTable | null;
    footnotes: FootnoteMaps | null;
    codeFence: {
        char: '`' | '~';
        length: number;
        bodyStart: number;
        state: {
            phase: 'leading' | 'run' | 'trailing' | 'invalid';
            indent: number;
            run: number;
            lineStart: number;
        };
    } | null;
    /** prior table tail can still revoke a provisional rowspan mutation */
    tableTailUnstable: boolean;
    /** cached table header and volatile-row source offsets */
    tableAppend: {
        prefixLen: number;
        lastRowStart: number;
        prefix: string;
        lastRow: string;
    } | null;
    /** leading source units and trailing source text removed in completion mode */
    leadingTrim: number;
    trailingTrim: string;
    lastCodeTextAppend?: ProvenAppend;
    lastPath: 'none' | 'full' | 'list-append' | 'table-append' | 'code-append';
    /** optional diagnostic observer invoked when incrementalLex does parser work */
    observeLex?: IncrementalLexObserver;
};
export type IncrementalLexPath = Exclude<IncrementalLexCache['lastPath'], 'none'>;
export type IncrementalLexObserver = (path: IncrementalLexPath, inputLength: number) => void;
