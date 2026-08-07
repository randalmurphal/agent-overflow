import type { Extension } from './index.js';
export declare const markedMath: Extension[];
export type MathToken = {
    type: 'math';
    raw: string;
    text: string;
    isInline: boolean;
    displayMode: boolean;
};
