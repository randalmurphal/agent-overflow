/**
 * What an authored image size (`<img width height>`, GitLab's
 * `{width=… height=…}`) paints as, as CSS lengths.
 *
 * A width is pixels or a percentage of the line; a height is pixels only,
 * since a percentage of an auto-height container is nothing. Anything else
 * is no size at all.
 *
 * A stylesheet's `height: auto` on images outranks the `height` attribute
 * (attributes are presentational hints), so a size is applied as a style.
 * With a pixel width and height the height is not set: the image takes
 * their ratio and keeps `height: auto`, so a line narrower than the width
 * scales both sides instead of squashing the picture.
 *
 * A percentage resolves against the line. An image whose wrapper shrinks to
 * fit it (`[data-streamdown-image]`) would resolve it against that wrapper,
 * so `wrapper` takes the percentage and the image fills the wrapper.
 *
 * `align` and `hspace` place the box as an `<img>`'s attributes do, both
 * forges keeping them: `left`/`right` float it, `top`/`middle`/`bottom` set
 * its place in the line, and `hspace` is pixels on each side. They apply to
 * the outermost box a renderer paints (the wrapper when there is one).
 */
const PIXELS = /^\d{1,5}$/;
const PERCENT = /^\d{1,3}%$/;

export interface ImageSize {
	/** The width of the image's wrapper: a percentage of the line. */
	wrapper?: string;
	width?: string;
	height?: string;
	/** `w / h` of an authored pixel width and height. */
	aspectRatio?: string;
	float?: 'left' | 'right';
	verticalAlign?: 'top' | 'middle' | 'bottom';
	/** Each side's margin, from `hspace`. */
	marginInline?: string;
}

const ALIGN = /^(left|right|top|middle|bottom)$/i;

/**
 * Reads `width`, `height`, `align` and `hspace` off an image token (EmbeddedImageToken) or any
 * holder of the two attributes; a plain markdown image has neither. With
 * `wrapped: false` (an `<img>` with no wrapper) a percentage is the image's
 * own width.
 */
export const imageSizeOf = (token: object, wrapped = true): ImageSize => {
	const { width, height, align, hspace } = token as { width?: unknown; height?: unknown; align?: unknown; hspace?: unknown };
	const pixelWidth = typeof width === 'string' && PIXELS.test(width) ? width : null;
	const percentWidth = typeof width === 'string' && PERCENT.test(width) ? width : null;
	const pixelHeight = typeof height === 'string' && PIXELS.test(height) ? height : null;
	const size: ImageSize = {};
	if (percentWidth !== null) {
		if (wrapped) {
			size.wrapper = percentWidth;
			size.width = '100%';
		} else {
			size.width = percentWidth;
		}
	} else if (pixelWidth !== null) {
		size.width = `${pixelWidth}px`;
	}
	if (pixelHeight !== null) {
		// A zero side has no ratio; it is painted as authored.
		if (pixelWidth !== null && Number(pixelWidth) > 0 && Number(pixelHeight) > 0) {
			size.aspectRatio = `${Number(pixelWidth)} / ${Number(pixelHeight)}`;
		} else {
			size.height = `${pixelHeight}px`;
		}
	}
	const placed = typeof align === 'string' ? ALIGN.exec(align.trim())?.[1].toLowerCase() : undefined;
	if (placed === 'left' || placed === 'right') size.float = placed;
	else if (placed !== undefined) size.verticalAlign = placed as ImageSize['verticalAlign'];
	if (typeof hspace === 'string' && PIXELS.test(hspace)) size.marginInline = `${hspace}px`;
	return size;
};
