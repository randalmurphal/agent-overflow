import type { Extension } from './index.js';
import type { Token } from 'marked';
export declare const markedAlign: Extension;
export declare const markedAlignBlock: Extension;
export declare const parseAlignSource: (src: string) => {
    raw: string;
    text: string;
    align: 'center' | 'right';
} | undefined;
export type AlignToken = {
    type: 'align';
    align: 'center' | 'right';
    raw: string;
    text: string;
    tokens: Token[];
};
