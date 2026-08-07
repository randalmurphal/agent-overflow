import type { Lexer } from 'marked';
import type { Extension, GenericToken } from './index.js';
export declare function letterToInt(letter: string): number;
export declare function romanToInt(roman: string): number;
export declare const romanUpper = "(?:C|XC|L?X{0,3}(?:IX|IV|V?I{0,3}))";
export declare const romanLower = "(?:c|xc|l?x{0,3}(?:ix|iv|v?i{0,3}))";
export declare const bulletPattern = "(?:[*+-]|(?:\\d{1,9}|[a-zA-Z]|(?:C|XC|L?X{0,3}(?:IX|IV|V?I{0,3}))|(?:c|xc|l?x{0,3}(?:ix|iv|v?i{0,3})))[.)])";
export declare const rule = "^( {0,3}(?:[*+-]|(?:\\d{1,9}|[a-zA-Z]|(?:C|XC|L?X{0,3}(?:IX|IV|V?I{0,3}))|(?:c|xc|l?x{0,3}(?:ix|iv|v?i{0,3})))[.)]))([ \\t][^\\n]*|[ \\t])?(?:\\n|$)";
/**
 * The single chokepoint for turning a list item's text into its child
 * tokens. Also rewrites the marker-line indented-code artifact (LLM prose
 * aligns values with spaces; CommonMark reads 5+ columns as code) — see the
 * source comment for the exact scope.
 */
export declare function tokenizeListItemContent(lexer: Lexer, item: ListItemToken, top: boolean): void;
export declare const markedList: Extension;
export interface ListToken {
    type: 'list';
    raw: string;
    ordered: boolean;
    listType: 'decimal' | 'lower-alpha' | 'upper-alpha' | 'lower-roman' | 'upper-roman' | null;
    loose: boolean;
    start?: number;
    tokens: ListItemToken[];
}
/**
 * Token representing an item in an extended list.
 */
export interface ListItemToken {
    type: 'list_item';
    raw: string;
    task: boolean;
    checked: boolean;
    loose: boolean;
    text: string;
    value: number | null;
    skipped: boolean;
    tokens: GenericToken[];
}
