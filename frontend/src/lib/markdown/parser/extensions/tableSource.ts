import type { Extension } from '../index';

/** Per-column alignment as the delimiter row declares it. */
export type TableAlignment = 'left' | 'center' | 'right' | null;

export interface ParsedTableSource {
    raw: string;
    headerRows: string[];
    bodyRows: string[];
    alignment: TableAlignment[];
    colCount: number;
    hasFooter: boolean;
}

export interface ParsedTableBlockSource {
    raw: string;
    headerRowCount: number;
    bodyRowCount: number;
    hasFooter: boolean;
}

/**
 * The raw grammar match plus the source the match was taken against, which
 * differs from the input when a CR retry was needed (see matchTableSource).
 */
interface TableSourceMatch {
    cap: RegExpExecArray;
    parsedSource: string;
    hasHeaderAlignment: boolean;
    raw: string;
}
const TABLE_START_WITH_HEADER = /^\n *([^\n ].*\|.*)\n/;
const TABLE_START_SIMPLE = /^\n *(\|.*\|)\n/;
const TABLE_WITH_ALIGNMENT = new RegExp('^' +
    '([^\\n ].*\\|.*\\n(?: *[^\\s].*\\n)*?)' +
    ' {0,3}(?:\\| *)?(:?-+:? *(?:\\| *:?-+:? *)*)(?:\\| *)?' +
    '(?:\\n((?:(?! *\\n| {0,3}((?:- *){3,}|(?:_ *){3,}|(?:\\* *){3,})' +
    '(?:\\n+|$)| {0,3}#{1,6} | {0,3}>| {4}[^\\n]| {0,3}(?:`{3,}' +
    '(?=[^`\\n]*\\n)|~{3,})[^\\n]*\\n| {0,3}(?:[*+-]|1[.)]) |' +
    '<\\/?(?:address|article|aside|base|basefont|blockquote|body|' +
    'caption|center|col|colgroup|dd|details|dialog|dir|div|dl|dt|fieldset|figcaption|figure|footer|form|frame|frameset|h[1-6]|head|header|hr|html|iframe|legend|li|link|main|menu|menuitem|meta|nav|noframes|ol|optgroup|option|p|param|section|source|summary|table|tbody|td|tfoot|th|thead|title|tr|track|ul)(?: +|\\n|\\/?>)|<(?:script|pre|style|textarea|!--)).*(?:\\n|$))*)\\n*|$)');
const SIMPLE_TABLE = /^(\|.*\|(?:\n\|.*\|)*)/;
const ALIGNMENT_ROW = /^ *(\| *)?:?-+:? *(\| *:?-+:? *)*(\| *)?$/;
const FOOTER_ROW = /^ *\| *:?-+:? *(\| *:?-+:? *)*\| *$/;
const RIGHT_ALIGNMENT = /^ *-+: *$/;
const CENTER_ALIGNMENT = /^ *:-+: *$/;
const LEFT_ALIGNMENT = /^ *:-+ *$/;

const normalizeCarriageReturns = (source: string): string => source.replace(/\r\n|\r/g, '\n');

const originalRawForNormalizedLength = (source: string, normalizedLength: number): string => {
    let sourceIndex = 0;
    let normalizedIndex = 0;
    while (normalizedIndex < normalizedLength && sourceIndex < source.length) {
        const current = source.charCodeAt(sourceIndex);
        const next = source.charCodeAt(sourceIndex + 1);
        sourceIndex += current === 13 && next === 10
            ? 2
            : 1;
        normalizedIndex++;
    }
    return source.slice(0, sourceIndex);
};

export const tableStart = (src: string): number | undefined => {
    if (src.charCodeAt(0) !== 10)
        return undefined;
    return TABLE_START_WITH_HEADER.exec(src)?.index ?? TABLE_START_SIMPLE.exec(src)?.index;
};

const processAlignment = (alignRow: string[]): TableAlignment[] => {
    const alignment: TableAlignment[] = new Array(alignRow.length);
    for (let i = 0; i < alignRow.length; i++) {
        const cell = alignRow[i];
        alignment[i] = RIGHT_ALIGNMENT.test(cell)
            ? 'right'
            : CENTER_ALIGNMENT.test(cell)
                ? 'center'
                : LEFT_ALIGNMENT.test(cell)
                    ? 'left'
                    : null;
    }
    return alignment;
};

const matchTableSource = (src: string): TableSourceMatch | null => {
    const firstLineEnd = src.indexOf('\n');
    const firstPipe = src.indexOf('|');
    if (firstPipe === -1 || (firstLineEnd !== -1 && firstPipe > firstLineEnd))
        return null;

    let parsedSource = src;
    let cap = TABLE_WITH_ALIGNMENT.exec(parsedSource);
    let hasHeaderAlignment = true;
    if (!cap) {
        cap = SIMPLE_TABLE.exec(parsedSource);
        hasHeaderAlignment = false;
    }
    // Lexer.lex normalizes carriage returns before invoking extensions, while
    // parseBlocks calls blockTokens directly to retain source offsets. Retry
    // when the LF grammar fails or stops before another pipe row, then map the
    // matched raw back to the source prefix so offsets remain byte-for-byte.
    const stoppedBeforePipeRow = cap && src.charCodeAt(cap[0].length) === 124;
    const stoppedAtCarriageReturn = cap && src.charCodeAt(cap[0].length) === 13;
    if ((!cap || stoppedBeforePipeRow || stoppedAtCarriageReturn) && src.includes('\r')) {
        const normalizedSource = normalizeCarriageReturns(src);
        let normalizedCap = TABLE_WITH_ALIGNMENT.exec(normalizedSource);
        let normalizedHasHeaderAlignment = true;
        if (!normalizedCap) {
            normalizedCap = SIMPLE_TABLE.exec(normalizedSource);
            normalizedHasHeaderAlignment = false;
        }
        if (normalizedCap) {
            parsedSource = normalizedSource;
            cap = normalizedCap;
            hasHeaderAlignment = normalizedHasHeaderAlignment;
        }
    }
    if (!cap)
        return null;

    return {
        cap,
        parsedSource,
        hasHeaderAlignment,
        raw: parsedSource === src
            ? cap[0]
            : originalRawForNormalizedLength(src, cap[0].length)
    };
};

/** Parse table boundaries and row geometry without allocating cell/token trees. */
export const parseTableSource = (src: string, detectFooter: boolean): ParsedTableSource | null => {
    const match = matchTableSource(src);
    if (!match)
        return null;
    const { cap, hasHeaderAlignment, raw } = match;

    let allTableContent = cap[1];
    if (cap[2])
        allTableContent += '\n' + cap[2];
    if (cap[3])
        allTableContent += '\n' + cap[3];
    const allRows = allTableContent.replace(/\n$/, '').split('\n');
    let headerRows: string[] = [];
    let bodyRows: string[] = [];
    let alignment: TableAlignment[] = [];
    let colCount = 0;
    if (hasHeaderAlignment) {
        let headerEndIndex = -1;
        let alignRow: string[] = [];
        for (let i = 0; i < allRows.length; i++) {
            const row = allRows[i].trim();
            if (!ALIGNMENT_ROW.test(row))
                continue;
            headerEndIndex = i;
            alignRow = row.replace(/^ *\| *| *\| *$/g, '').split(/ *\| */);
            break;
        }
        if (headerEndIndex === -1) {
            bodyRows = allRows;
            colCount = bodyRows[0].split('|').filter((cell) => cell.trim() !== '').length;
            alignment = new Array(colCount).fill(null);
        }
        else {
            headerRows = allRows.slice(0, headerEndIndex).filter((row) => row.trim() !== '');
            bodyRows = headerEndIndex + 1 < allRows.length ? allRows.slice(headerEndIndex + 1) : [];
            colCount = alignRow.length;
            if (colCount === 0)
                return null;
            alignment = processAlignment(alignRow);
        }
    }
    else {
        bodyRows = allRows;
        colCount = bodyRows[0].split('|').filter((cell) => cell.trim() !== '').length;
        alignment = new Array(colCount).fill(null);
    }

    let hasFooter = false;
    let processedBodyRows = bodyRows;
    if (detectFooter && hasHeaderAlignment && bodyRows.length > 0) {
        for (let i = bodyRows.length - 1; i >= 0; i--) {
            if (!FOOTER_ROW.test(bodyRows[i]))
                continue;
            hasFooter = true;
            processedBodyRows = bodyRows.slice(0, i).concat(bodyRows.slice(i + 1));
            break;
        }
    }
    return {
        raw,
        headerRows,
        bodyRows: processedBodyRows,
        alignment,
        colCount,
        hasFooter
    };
};

export const parseTableBlockSource = (
    src: string,
    detectFooter: boolean
): ParsedTableBlockSource | null => {
    const match = matchTableSource(src);
    if (!match)
        return null;

    let headerRowCount = 0;
    let bodySourceRowCount = 0;
    let alignmentSeen = !match.hasHeaderAlignment;
    let hasFooter = false;
    let lineStart = 0;
    while (lineStart < match.cap[0].length) {
        const newline = match.cap[0].indexOf('\n', lineStart);
        const lineEnd = newline === -1 ? match.cap[0].length : newline;
        const line = match.cap[0].slice(lineStart, lineEnd).trim();
        lineStart = newline === -1 ? match.cap[0].length : newline + 1;
        if (line.length === 0)
            continue;
        if (!alignmentSeen) {
            if (ALIGNMENT_ROW.test(line)) {
                alignmentSeen = true;
            }
            else {
                headerRowCount++;
            }
            continue;
        }
        bodySourceRowCount++;
        if (detectFooter && match.hasHeaderAlignment && FOOTER_ROW.test(line))
            hasFooter = true;
    }

    // Defensive parity with parseTableSource: if the alignment-shaped match
    // contained no alignment line, every row is body content.
    if (!alignmentSeen) {
        bodySourceRowCount = headerRowCount;
        headerRowCount = 0;
    }
    const bodyRowCount = hasFooter
        ? Math.max(0, bodySourceRowCount - 2)
        : bodySourceRowCount;
    return { raw: match.raw, headerRowCount, bodyRowCount, hasFooter };
};

export const markedTableBlock: Extension = {
    name: 'table',
    level: 'block',
    start: tableStart,
    tokenizer(src) {
        const table = parseTableBlockSource(src, true);
        if (!table)
            return undefined;
        return {
            type: 'table',
            raw: table.raw,
            headerRowCount: table.headerRowCount,
            bodyRowCount: table.bodyRowCount,
            hasFooter: table.hasFooter
        };
    }
};
