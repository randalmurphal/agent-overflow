/**
 * A label chip: a label's title on its color, as GitLab draws `~bug`. A
 * scoped title (`type::bug`) reads as two parts, the scope on the color and
 * the value beside it, inside a border of the color. A host's extension
 * builds the token; this library knows only how one renders.
 *
 * The chip's box depends on its text alone: a chip without colors (the
 * host does not know them yet) has the same box in the theme's neutral
 * colors. Colors are data, so each renderer writes a style only for a
 * value `labelChipColor` accepts.
 */
export type LabelChipToken = {
	type: 'labelChip';
	raw: string;
	/** What the chip reads, its parts joined. */
	text: string;
	/** A scoped title's scope (everything before its last `::`), or null. */
	scope: string | null;
	/** The value of a scoped title, else the whole title. */
	name: string;
	/** Where the chip links; the renderers' link rule decides whether it may. */
	href: string;
	title: string | null;
	/** `#rrggbb`, or null for the neutral chip. */
	color: string | null;
	textColor: string | null;
};

const HEX_COLOR = /^#[0-9a-f]{6}$/i;

/** `value` when it is a `#rrggbb` color, the only form a style takes. */
export const labelChipColor = (value: unknown): string | null =>
	typeof value === 'string' && HEX_COLOR.test(value) ? value : null;

/** The chip's border, in its color. */
export const labelChipBorderStyle = (token: LabelChipToken): string | null => {
	const color = labelChipColor(token.color);
	return color ? `border-color: ${color};` : null;
};

/** The colored part: the scope of a scoped chip, else the whole chip. */
export const labelChipKeyStyle = (token: LabelChipToken): string | null => {
	const color = labelChipColor(token.color);
	if (!color) return null;
	const text = labelChipColor(token.textColor);
	return `background-color: ${color};${text ? ` color: ${text};` : ''}`;
};
