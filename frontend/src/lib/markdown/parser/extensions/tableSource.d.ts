import type { Extension } from './index.js';

export interface ParsedTableSource {
    raw: string;
    headerRows: string[];
    bodyRows: string[];
    alignment: Array<'left' | 'center' | 'right' | null>;
    colCount: number;
    hasFooter: boolean;
}

export interface ParsedTableBlockSource {
    raw: string;
    headerRowCount: number;
    bodyRowCount: number;
    hasFooter: boolean;
}

export declare const tableStart: (src: string) => number | undefined;
export declare const parseTableSource: (
    src: string,
    detectFooter: boolean,
) => ParsedTableSource | null;
export declare const parseTableBlockSource: (
    src: string,
    detectFooter: boolean,
) => ParsedTableBlockSource | null;
export declare const markedTableBlock: Extension;
