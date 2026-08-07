import type { Extension } from './index.js';
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
    tokens?: unknown[];
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
interface WorkingCell extends BaseCell {
}
type WorkingRow = WorkingCell[];
export declare const DEFAULT_OPTIONS: Required<SpanTableOptions>;
export declare const getTableCell: (text: string, cell: BaseCell, type: "th" | "td", align: string | null) => string;
export declare const splitCells: (tableRow: string, count: number | null, prevRow?: WorkingRow | null, maxColspan?: number | null) => WorkingRow;
export declare const markedTable: Extension;
export {};
