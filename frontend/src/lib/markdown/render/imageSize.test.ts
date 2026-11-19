import { describe, expect, it } from 'vitest';
import { imageSizeOf } from './imageSize';

describe('imageSizeOf', () => {
	it('paints a pixel width and height as the width and their ratio, so the height follows a narrowed width', () => {
		expect(imageSizeOf({ width: '400', height: '100' })).toEqual({ width: '400px', aspectRatio: '400 / 100' });
		expect(imageSizeOf({ width: '400', height: '0100' }, false)).toEqual({ width: '400px', aspectRatio: '400 / 100' });
	});

	it('paints a zero side as authored, since it has no ratio', () => {
		expect(imageSizeOf({ width: '0', height: '10' })).toEqual({ width: '0px', height: '10px' });
	});

	it('gives a percentage to the wrapper, which the image fills', () => {
		expect(imageSizeOf({ width: '50%', height: '50%' })).toEqual({ wrapper: '50%', width: '100%' });
		expect(imageSizeOf({ width: '50%', height: '30' })).toEqual({ wrapper: '50%', width: '100%', height: '30px' });
	});

	it('gives a percentage to the image itself when it has no wrapper', () => {
		expect(imageSizeOf({ width: '50%' }, false)).toEqual({ width: '50%' });
	});

	it('reads each side on its own', () => {
		expect(imageSizeOf({ width: null, height: '20' })).toEqual({ height: '20px' });
		expect(imageSizeOf({ width: '16' })).toEqual({ width: '16px' });
	});

	it.each([
		['a unit', '12px'],
		['a declaration', '1;position:fixed'],
		['a function', 'calc(100% + 1px)'],
		['a negative', '-5'],
		['a decimal', '1.5'],
		['an empty value', ''],
		['an absurd length', '123456'],
		['a number, not a string', 120],
	])('gives no size for %s', (_name, value) => {
		expect(imageSizeOf({ width: value, height: value })).toEqual({});
	});

	it('floats a left or right align, places a top, middle or bottom one in the line, and spaces by hspace', () => {
		expect(imageSizeOf({ align: 'right', hspace: '10' })).toEqual({ float: 'right', marginInline: '10px' });
		expect(imageSizeOf({ align: ' Left ' })).toEqual({ float: 'left' });
		expect(imageSizeOf({ align: 'middle' })).toEqual({ verticalAlign: 'middle' });
		expect(imageSizeOf({ align: 'top', width: '16' })).toEqual({ verticalAlign: 'top', width: '16px' });
	});

	it.each([['center'], ['justify'], ['left;position:fixed'], [''], [null]])('places nothing for align %o', (align) => {
		expect(imageSizeOf({ align, hspace: '1em' })).toEqual({});
	});

	it('gives no size for a token with neither attribute', () => {
		expect(imageSizeOf({ type: 'image', href: 'https://x/a.png' })).toEqual({});
	});
});
