/**
 * GitLab's JSON tables: a fenced block whose info string is `json:table`
 * and whose body is a JSON object with an `items` array renders as a table.
 * Opt-in: GitHub, CommonMark and agent chat show the fence as code.
 *
 * `fields` names the columns, each a key or `{ "key", "label" }`; without
 * it the columns are the first item's keys. Each item is a row and each
 * cell its value for the column's key, as text; with `"markdown": true` a
 * string value is inline Markdown. `caption` is the table's caption. A body
 * that is not such an object stays a code block. GitLab's page also lets a
 * reader filter (`filter`) and sort (`sortable`) the table; this renders
 * the data only.
 *
 * The token is the extended `table` token every renderer and find's
 * projection already show, with a `caption` section first when there is
 * one. The fence's extent is the engine's, so block splitting needs nothing.
 */
import type { Token, Tokens } from '../engine';
import type { Extension } from '../lexer';
import type { TableSection, TableToken, TD, TH } from './table';

type Field = { key: string; label: string };
type JsonTable = { fields: Field[]; items: unknown[]; markdown: boolean; caption: string | null };

const isRecord = (value: unknown): value is Record<string, unknown> =>
	typeof value === 'object' && value !== null && !Array.isArray(value);

const readTable = (text: string): JsonTable | null => {
	let data: unknown;
	try {
		data = JSON.parse(text);
	} catch {
		return null;
	}
	if (!isRecord(data) || !Array.isArray(data.items)) return null;
	const named = Array.isArray(data.fields) ? data.fields : [];
	const fields: Field[] = [];
	for (const field of named) {
		if (typeof field === 'string') fields.push({ key: field, label: field });
		else if (isRecord(field) && typeof field.key === 'string')
			fields.push({ key: field.key, label: typeof field.label === 'string' ? field.label : field.key });
	}
	if (fields.length === 0 && isRecord(data.items[0]))
		for (const key of Object.keys(data.items[0])) fields.push({ key, label: key });
	return {
		fields,
		items: data.items,
		markdown: data.markdown === true,
		caption: typeof data.caption === 'string' ? data.caption : null
	};
};

/** A cell value as the text it shows. */
const cellText = (value: unknown): string => {
	if (value === undefined || value === null) return '';
	if (typeof value === 'string') return value;
	if (typeof value === 'number' || typeof value === 'boolean') return String(value);
	return JSON.stringify(value);
};

const plain = (text: string): Token[] => {
	if (text === '') return [];
	const token: Tokens.Text = { type: 'text', raw: text, text };
	return [token];
};

export const markedJsonTable: Extension = {
	name: 'jsonTable',
	level: 'block',
	tokenizer(src) {
		let lead = 0;
		while (lead < 3 && src.charCodeAt(lead) === 32) lead++;
		const first = src.charCodeAt(lead);
		if (first !== 96 /* ` */ && first !== 126 /* ~ */) return undefined;
		const fence = this.lexer.tokenizer.fences(src);
		if (!fence || fence.lang?.split(/\s/, 1)[0] !== 'json:table') return undefined;
		const table = readTable(fence.text);
		if (!table) return undefined;
		const inline = (text: string): Token[] => (table.markdown ? this.lexer.inline(text, []) : plain(text));
		const header: TH[] = table.fields.map((field, position) => ({
			type: 'th',
			rowspan: 1,
			colspan: 1,
			text: field.label,
			position,
			align: null,
			tokens: plain(field.label)
		}));
		const rows = table.items.map((item) => ({
			type: 'tr' as const,
			tokens: table.fields.map((field, position): TD => {
				const text = cellText(isRecord(item) ? item[field.key] : undefined);
				return { type: 'td', rowspan: 1, colspan: 1, text, position, align: null, tokens: inline(text) };
			})
		}));
		const sections: TableSection[] = [];
		if (table.caption !== null) sections.push({ type: 'caption', tokens: inline(table.caption) });
		sections.push({ type: 'thead', tokens: [{ type: 'tr', tokens: header }] });
		if (rows.length > 0) sections.push({ type: 'tbody', tokens: rows });
		const token: TableToken = {
			type: 'table',
			raw: fence.raw,
			tokens: sections,
			align: table.fields.map(() => null)
		};
		return token;
	}
};
