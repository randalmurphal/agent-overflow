import { type StreamdownToken, type Extension } from './index.js';
export declare function markedFootnote(): Extension[];
export declare const markedFootnoteBlock: Extension;
/**
 * Represents a single footnote.
 */
export type Footnote = {
    type: 'footnote';
    raw: string;
    label: string;
    tokens: StreamdownToken[];
    lines: string[];
};
/**
 * Represents a reference to a footnote.
 */
export type FootnoteRef = {
    type: 'footnoteRef';
    raw: string;
    label: string;
    content: Footnote;
};
export type FootnoteToken = Footnote | FootnoteRef;
