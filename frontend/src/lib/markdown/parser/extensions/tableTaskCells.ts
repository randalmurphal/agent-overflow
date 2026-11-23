/**
 * GitLab's task boxes in tables: a pipe-table cell holding only `[ ]`, `[x]`,
 * `[X]` or `[~]` renders a disabled checkbox, checked for `x`. Opt-in:
 * GitHub, CommonMark and agent chat show the brackets as text, and a box
 * anywhere but alone in a cell is text on GitLab too.
 */
import type { Token } from '../engine';
import type { Extension } from '../lexer';
import { isTableCellRun } from './table';

const TASK_CELL = /^\[([ xX~])\]$/;

/** A disabled task checkbox outside a list item. */
export type CheckboxToken = {
	type: 'checkbox';
	raw: string;
	checked: boolean;
};

export const markedTableTaskCells: Extension = {
	name: 'tableTaskCells',
	level: 'inline',
	// Ahead of the default citation rule, which reads `[~]` as a citation.
	precedesDefaults: true,
	tokenizer(src, tokens) {
		if (src.charCodeAt(0) !== 91 /* [ */ || tokens.length > 0 || !isTableCellRun(tokens as Token[])) return undefined;
		const match = TASK_CELL.exec(src);
		if (!match) return undefined;
		const checkbox: CheckboxToken = { type: 'checkbox', raw: src, checked: match[1] === 'x' || match[1] === 'X' };
		return checkbox;
	}
};
