import type { Extension } from '../index';
import type { Lexer, Token } from '../engine';
import type { TableAlignment } from './tableSource';

export interface SpanTableOptions {
    useTheadTbody?: boolean;
    useTfoot?: boolean;
    detectFooter?: boolean;
    maxColspan?: number | null;
    handleComplexSpans?: boolean;
}
interface BaseCell {
    rowspan: number;
    colspan: number;
    text: string;
    position?: number;
    tokens?: Token[];
    rowSpanTarget?: BaseCell;
    complexRowSpan?: boolean;
    relatedCell?: BaseCell;
    align?: string | null;
}
export interface TH extends BaseCell {
    type: 'th';
}
export interface TD extends BaseCell {
    type: 'td';
}
export interface THeadRow {
    type: 'tr';
    tokens: TH[];
}
export interface TRow {
    type: 'tr';
    tokens: TD[];
}
export interface THead {
    type: 'thead';
    tokens: THeadRow[];
}
export interface TBody {
    type: 'tbody';
    tokens: TRow[];
}
export interface TFoot {
    type: 'tfoot';
    tokens: TRow[];
}
export type TableSection = THead | TBody | TFoot;
export interface TableToken {
    type: 'table';
    tokens: TableSection[];
    raw: string;
    align: (string | null)[];
}
/** A cell mid-processing, before it is stamped `th` or `td`. */
type WorkingCell = BaseCell;
type WorkingRow = WorkingCell[];
import { parseTableSource, tableStart } from './tableSource';
// Default configuration options for the extended tables extension
export const DEFAULT_OPTIONS: Required<SpanTableOptions> = {
    useTheadTbody: true,
    useTfoot: false,
    detectFooter: true,
    maxColspan: null,
    handleComplexSpans: true
};
// Creates an HTML table cell with appropriate attributes
export const getTableCell = (
    text: string,
    cell: BaseCell,
    type: 'th' | 'td',
    align: string | null
): string => {
    if (!cell.rowspan)
        return '';
    const tag = `<${type}` +
        `${cell.colspan > 1 ? ` colspan=${cell.colspan}` : ''}` +
        `${cell.rowspan > 1 ? ` rowspan=${cell.rowspan}` : ''}` +
        `${align ? ` align=${align}` : ''}>`;
    return `${tag + text}</${type}>\n`;
};
function splitRow(src: string): string[] {
    const out: string[] = [];
    let buf = '';
    let esc = false;
    let inCode = false;
    let fence = 0;
    for (let i = 0; i < src.length; i++) {
        const ch = src[i];
        if (esc) {
            buf += ch;
            esc = false;
            continue;
        }
        if (ch === '\\') {
            esc = true;
            buf += ch;
            continue;
        }
        if (ch === '`') {
            let run = 1;
            while (i + run < src.length && src[i + run] === '`')
                run++;
            if (!inCode) {
                inCode = true;
                fence = run;
            }
            else if (run >= fence) {
                inCode = false;
                fence = 0;
            }
            buf += src.slice(i, i + run);
            i += run - 1;
            continue;
        }
        if (ch === '|' && !inCode) {
            // Count consecutive pipes for colspan
            let consecutivePipes = 1;
            while (i + consecutivePipes < src.length && src[i + consecutivePipes] === '|') {
                consecutivePipes++;
            }
            if (consecutivePipes > 1) {
                // Multiple pipes = colspan marker
                out.push(buf.trim() + '\x00COLSPAN:' + consecutivePipes);
                i += consecutivePipes - 1;
            }
            else {
                out.push(buf.trim());
            }
            buf = '';
            continue;
        }
        buf += ch;
    }
    out.push(buf.trim());
    return out;
}
// Splits a table row into cells and processes row/column spans
export const splitCells = (
    tableRow: string,
    count: number | null,
    prevRow: WorkingRow | null = null,
    maxColspan: number | null = null
): WorkingRow => {
    // Split by pipe, but handle escaped pipes and empty cells
    const cells = splitRow(tableRow);
    // Remove first/last cell if it's empty (from leading/trailing pipes)
    if (cells.length > 0 && !cells[0])
        cells.shift();
    if (cells.length > 0 && !cells[cells.length - 1])
        cells.pop();
    return processSpans(cells, count, prevRow || [], maxColspan);
};
// Process row and column spans in table cells
const processSpans = (
    cells: string[],
    count: number | null,
    prevRow: WorkingRow = [],
    maxColspan: number | null = null
): WorkingRow => {
    let numCols = 0;
    let i: number, j: number, trimmedCell: string, prevCell: WorkingCell;
    const processedCells: WorkingRow = [];
    // Track colspan cells that need rowspan
    let colspanCells: Map<string, { original: WorkingCell; newCell: WorkingCell }> | null = null;
    // First pass: Process each cell's colspan
    let cellIndex = 0;
    for (i = 0; i < cells.length; i++) {
        trimmedCell = cells[i];
        let colspan = 1;
        // Check for colspan marker from consecutive pipes
        if (trimmedCell.includes('\x00COLSPAN:')) {
            const parts = trimmedCell.split('\x00COLSPAN:');
            trimmedCell = parts[0];
            colspan = parseInt(parts[1], 10);
        }
        if (maxColspan !== null && colspan > maxColspan)
            colspan = maxColspan;
        processedCells[cellIndex] = {
            rowspan: 1,
            colspan: colspan,
            text: trimmedCell.trim().replace(/\\\|/g, '|'),
            position: numCols
        };
        numCols += processedCells[cellIndex].colspan;
        cellIndex++;
    }
    // Second pass: Process rowspan by matching cells by position
    for (i = 0; i < processedCells.length; i++) {
        const cell = processedCells[i];
        let cellText = cell.text;
        // Handle Rowspan - cells ending with ^ (but not superscript ^text^)
        // Check if it's a rowspan indicator (single ^ at end) vs superscript (^text^)
        const isRowspanIndicator = cellText.slice(-1) === '^' && !cellText.match(/\^[^^\n\r]+\^$/); // Not a superscript pattern ^text^
        if (isRowspanIndicator && prevRow.length > 0) {
            // Clean the ^ indicator from the cell text. A cell that is nothing but
            // carets (the usual `^^` continuation marker) carries no content.
            cell.text = cellText.slice(0, -1).trim();
            if (/^\^*$/.test(cell.text))
                cell.text = '';
            cellText = cell.text;
            const startPosition = cell.position || 0;
            const endPosition = startPosition + cell.colspan - 1;
            // Try to find a matching cell or combination of cells in previous row
            for (j = 0; j < prevRow.length; j++) {
                prevCell = prevRow[j];
                const prevStartPosition = prevCell.position || 0;
                const prevEndPosition = prevStartPosition + prevCell.colspan - 1;
                // Check for position overlap between cells
                if ((startPosition >= prevStartPosition && startPosition <= prevEndPosition) ||
                    (endPosition >= prevStartPosition && endPosition <= prevEndPosition) ||
                    (prevStartPosition >= startPosition && prevEndPosition <= endPosition)) {
                    // Complex case: Handle rowspan for colspan cells
                    if (cell.colspan > 1 && prevCell.colspan > 1) {
                        // If the cell spans exactly match, simple case
                        if (cell.colspan === prevCell.colspan && cell.position === prevCell.position) {
                            cell.rowSpanTarget = prevCell.rowSpanTarget ?? prevCell;
                            // Only append text if it's different from the target cell.
                            // cell.text was already cleaned of its ^ indicator above —
                            // slicing again here used to drop the last real character.
                            const textToAppend = cell.text.trim();
                            const targetText = cell.rowSpanTarget.text.trim();
                            // Don't append if the text is the same or already contained (common case for rowspan indicators)
                            if (textToAppend &&
                                textToAppend !== targetText &&
                                !targetText.includes(textToAppend)) {
                                cell.rowSpanTarget.text = targetText + (targetText ? ' ' : '') + textToAppend;
                            }
                            cell.rowSpanTarget.rowspan += 1;
                            cell.rowspan = 0;
                            break;
                        }
                        else {
                            // More complex case: Track colspan cells that need rowspan for next row
                            const key = `${cell.position}-${cell.colspan}`;
                            colspanCells ??= new Map();
                            colspanCells.set(key, {
                                original: prevCell,
                                newCell: cell
                            });
                            // Keep the cell visible for now, will be merged in rendering
                        }
                    }
                    else {
                        // Standard case of single column cell with rowspan
                        cell.rowSpanTarget = prevCell.rowSpanTarget ?? prevCell;
                        // Only append text if it's different from the target cell.
                        // cell.text was already cleaned of its ^ indicator above.
                        const textToAppend = cell.text.trim();
                        const targetText = cell.rowSpanTarget.text.trim();
                        // Don't append if the text is the same or already contained (common case for rowspan indicators)
                        if (textToAppend && textToAppend !== targetText && !targetText.includes(textToAppend)) {
                            cell.rowSpanTarget.text = targetText + (targetText ? ' ' : '') + textToAppend;
                        }
                        cell.rowSpanTarget.rowspan += 1;
                        cell.rowspan = 0;
                        break;
                    }
                }
            }
            // No target found: cell.text was already cleaned of its ^ indicator
            // above, so the cell simply renders as a normal cell.
        }
    }
    // Process any complex colspan+rowspan combinations we tracked
    if (colspanCells) {
        for (const { original, newCell } of colspanCells.values()) {
            if (original && newCell) {
                // Here we could apply more sophisticated merging logic
                // For now, just mark that these cells have a relationship
                newCell.complexRowSpan = true;
                newCell.relatedCell = original;
            }
        }
    }
    // Normalize column count
    return normalizeColumnCount(processedCells, count, numCols);
};
// Ensures the row has the correct number of columns
const normalizeColumnCount = (cells: WorkingRow, count: number | null, numCols: number): WorkingRow => {
    // If count is null, don't normalize
    if (count === null)
        return cells;
    if (numCols > count) {
        // We need to keep track of total column count
        let currentColCount = 0;
        const cellsToKeep: WorkingRow = [];
        for (const cell of cells) {
            if (currentColCount + cell.colspan <= count) {
                // This cell fits completely
                cellsToKeep.push(cell);
                currentColCount += cell.colspan;
            }
            else if (currentColCount < count) {
                // This cell partially fits - adjust its colspan
                const adjustedCell = { ...cell };
                adjustedCell.colspan = count - currentColCount;
                cellsToKeep.push(adjustedCell);
                currentColCount = count;
            }
            else {
                // This cell doesn't fit at all
                break;
            }
        }
        return cellsToKeep;
    }
    else {
        while (numCols < count) {
            cells.push({
                colspan: 1,
                rowspan: 1,
                text: '',
                position: numCols
            });
            numCols += 1;
        }
    }
    return cells;
};
// Convert working cell to TH
function workingCellToTH(cell: WorkingCell, align: TableAlignment): TH {
    return {
        type: 'th',
        rowspan: cell.rowspan,
        colspan: cell.colspan,
        text: cell.text,
        position: cell.position,
        tokens: cell.tokens,
        rowSpanTarget: cell.rowSpanTarget,
        complexRowSpan: cell.complexRowSpan,
        relatedCell: cell.relatedCell,
        align
    };
}
// Convert working cell to TD
function workingCellToTD(cell: WorkingCell, align: TableAlignment): TD {
    return {
        type: 'td',
        rowspan: cell.rowspan,
        colspan: cell.colspan,
        text: cell.text,
        position: cell.position,
        tokens: cell.tokens,
        rowSpanTarget: cell.rowSpanTarget,
        complexRowSpan: cell.complexRowSpan,
        relatedCell: cell.relatedCell,
        align
    };
}
function splitTableRows(
    rows: string[],
    colCount: number | null,
    previousRow: WorkingRow | null,
    maxColspan: number | null
): WorkingRow[] {
    const processed: WorkingRow[] = new Array(rows.length);
    for (let i = 0; i < rows.length; i++) {
        const previous = i > 0 ? processed[i - 1] : previousRow;
        processed[i] = splitCells(rows[i], colCount, previous, maxColspan);
    }
    return processed;
}
function tableRowTokens<Header extends boolean>(
    rows: WorkingRow[],
    alignment: TableAlignment[],
    lexer: Lexer,
    header: Header,
    eager: boolean
): Header extends true ? THeadRow[] : TRow[] {
    const result = new Array(rows.length);
    for (let rowIndex = 0; rowIndex < rows.length; rowIndex++) {
        const row = rows[rowIndex];
        const cells: Array<TH | TD> = new Array(row.length);
        for (let cellIndex = 0; cellIndex < row.length; cellIndex++) {
            const working = row[cellIndex];
            const cellAlignment = working.position !== undefined ? alignment[working.position] : null;
            const cell = header
                ? workingCellToTH(working, cellAlignment)
                : workingCellToTD(working, cellAlignment);
            cell.tokens = eager
                ? lexer.inlineTokens(cell.text, cell.tokens)
                : lexer.inline(cell.text, cell.tokens);
            cells[cellIndex] = cell;
        }
        result[rowIndex] = { type: 'tr', tokens: cells };
    }
    return result;
}
// Process table rows and add inline tokens to cells
function processRows(
    headerRows: string[],
    bodyRows: string[],
    alignment: TableAlignment[],
    colCount: number,
    lexer: Lexer,
    maxColspan: number | null,
    detectFooter: boolean
): TableSection[] {
    const tokens: TableSection[] = [];
    const processedHeaderRows = splitTableRows(headerRows, colCount, null, maxColspan);
    // Convert header rows to THead (only if we have header rows)
    if (processedHeaderRows.length > 0) {
        tokens.push({
            type: 'thead',
            tokens: tableRowTokens(processedHeaderRows, alignment, lexer, true, false)
        });
    }
    // Process body rows
    if (bodyRows.length > 0) {
        const processedBodyRows = splitTableRows(
            bodyRows,
            colCount,
            processedHeaderRows[processedHeaderRows.length - 1] ?? null,
            maxColspan
        );
        // Handle footer detection
        let tbodyRows = processedBodyRows;
        let tfootRows: WorkingRow[] = [];
        if (detectFooter && processedBodyRows.length > 0) {
            const lastRowIndex = processedBodyRows.length - 1;
            tfootRows = [processedBodyRows[lastRowIndex]];
            tbodyRows = processedBodyRows.slice(0, lastRowIndex);
        }
        // Convert body rows to TBody if there are any
        if (tbodyRows.length > 0) {
            tokens.push({
                type: 'tbody',
                tokens: tableRowTokens(tbodyRows, alignment, lexer, false, false)
            });
        }
        // Convert footer rows to TFoot if there are any
        if (tfootRows.length > 0) {
            tokens.push({
                type: 'tfoot',
                tokens: tableRowTokens(tfootRows, alignment, lexer, false, false)
            });
        }
    }
    return tokens;
}
const { detectFooter, maxColspan } = DEFAULT_OPTIONS;
const TAIL_ROWSPAN_CARET = /\^[ \t]*(\||$)/;
export const tokenizeTableTail = (
    src: string,
    lexer: Lexer
): { raw: string; align: TableAlignment[]; headerRowCount: number; rows: TRow[] } | null => {
    const table = parseTableSource(src, detectFooter);
    if (!table || table.raw !== src || table.hasFooter || table.headerRows.length === 0 || table.bodyRows.length === 0)
        return null;
    for (const row of table.bodyRows) {
        if (TAIL_ROWSPAN_CARET.test(row))
            return null;
    }
    const rows = splitTableRows(table.bodyRows, table.colCount, null, maxColspan);
    return {
        raw: table.raw,
        align: table.alignment,
        headerRowCount: table.headerRows.length,
        rows: tableRowTokens(rows, table.alignment, lexer, false, true)
    };
};
// Adds support for extended tables in marked with row spanning, column spanning,
// multi-row headers, and column alignment
export const markedTable: Extension = {
    name: 'table',
    level: 'block',
    start: tableStart,
    tokenizer(src) {
        const table = parseTableSource(src, detectFooter);
        if (!table)
            return undefined;
        const tokens = processRows(
            table.headerRows,
            table.bodyRows,
            table.alignment,
            table.colCount,
            this.lexer,
            maxColspan,
            table.hasFooter
        );
        return {
            type: 'table',
            tokens,
            raw: table.raw,
            align: table.alignment
        };
    }
};
