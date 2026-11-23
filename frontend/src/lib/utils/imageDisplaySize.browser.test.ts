import { afterEach, describe, expect, it } from 'vitest';

// Chromium's sizing of the images whose size the backend answers from a
// header reader (internal/attachment: SVGDisplaySize, AVIFSize): every
// answer the Go tests assert is what Chromium lays the image out at. Go
// and TypeScript share no table, so each row states the answer its Go row
// asserts.

const NS = 'xmlns="http://www.w3.org/2000/svg"';
const svgSrc = (attrs: string) => `data:image/svg+xml;base64,${btoa(`<svg ${NS} ${attrs}><rect width="10" height="10"/></svg>`)}`;

async function loaded(src: string, style: Partial<CSSStyleDeclaration> = {}): Promise<HTMLImageElement> {
  const host = document.createElement('div');
  host.style.width = '600px';
  const img = document.createElement('img');
  Object.assign(img.style, style);
  img.src = src;
  host.appendChild(img);
  document.body.appendChild(host);
  await img.decode();
  return img;
}

afterEach(() => {
  document.body.innerHTML = '';
});

describe('an svg with no authored size', () => {
  // TestSVGDisplaySize's definite rows: Chromium's natural size.
  it.each([
    ['numbers', 'width="120" height="80"', 120, 80],
    ['units in any case', 'width="5PX" height="6Px"', 5, 6],
    ['decimals round', 'width="10.4" height="10.5"', 10, 11],
    ['exponent and padding', 'width=" 1.2e2 " height="8E1"', 120, 80],
    ['in', 'width="1in" height="0.5IN"', 96, 48],
    ['cm and mm', 'width="2.54cm" height="25.4mm"', 96, 96],
    ['Q', 'width="40Q" height="10q"', 38, 9],
    ['pt and pc', 'width="72pt" height="6pc"', 96, 96],
    ['a viewBox does not change both definite sides', 'viewBox="0 0 1 1" width="3" height="4"', 3, 4],
    ['only width', 'width="100"', 100, 150],
    ['only height', 'height="80"', 300, 80],
    ['percent width', 'width="50%" height="50"', 300, 50],
    ['auto width', 'width="auto" height="50"', 300, 50],
    ['width and a ratio', 'width="100" viewBox="0 0 200 100"', 100, 50],
    ['height and a ratio', 'height="100" viewBox="0 0 200 100"', 200, 100],
    ['width and a ratio rounds', 'width="10" viewBox="0 0 3 1"', 10, 3],
    ['both sides and preserveAspectRatio none', 'width="100" height="40" viewBox="0 0 200 100" preserveAspectRatio="none"', 100, 40],
    ['no size', '', 300, 150],
    ['percent both', 'width="100%" height="50%"', 300, 150],
    ['uppercase attribute names are other attributes', 'WIDTH="5" HEIGHT="6"', 300, 150],
    ['zero viewBox width', 'viewBox="0 0 0 100"', 300, 150],
    ['negative viewBox width', 'viewBox="0 0 -200 100"', 300, 150],
  ])('%s', async (_name, attrs, width, height) => {
    const img = await loaded(svgSrc(attrs));
    expect([img.naturalWidth, img.naturalHeight]).toEqual([width, height]);
    // Laid out at that size too, within a pixel (a fractional length lays
    // out unrounded; the app paints the backend's integers as attributes).
    const box = img.getBoundingClientRect();
    expect(Math.abs(box.width - width)).toBeLessThan(1);
    expect(Math.abs(box.height - height)).toBeLessThan(1);
  });

  // TestSVGDisplaySize's fluid rows: no natural size, only the ratio, so a
  // block fills its container at that ratio, and the ratio is the terms'.
  it.each([
    ['only viewBox', 'viewBox="0 0 200 100"', 2, 1],
    ['viewBox with commas', 'viewBox="0,0,200,100"', 2, 1],
    ['viewBox with commas and spaces', 'viewBox=" 0, 0 ,200 , 100 "', 2, 1],
    ['percent both and a viewBox', 'width="100%" height="100%" viewBox="0 0 64 48"', 4, 3],
    ['square viewBox', 'viewBox="0 0 24 24"', 1, 1],
    ['fractional viewBox', 'viewBox="0 0 16.5 9"', 11, 6],
    ['preserveAspectRatio keeps the ratio', 'viewBox="0 0 200 100" preserveAspectRatio="xMinYMax slice"', 2, 1],
  ])('%s is fluid', async (_name, attrs, width, height) => {
    const img = await loaded(svgSrc(attrs), { display: 'block' });
    const box = img.getBoundingClientRect();
    expect(Math.abs(box.width - 600)).toBeLessThan(0.1);
    expect(Math.abs(box.height - (600 * height) / width)).toBeLessThan(0.1);
    // And a blank svg of the terms' ratio lays out the same.
    const blank = await loaded(`data:image/svg+xml,${encodeURIComponent(`<svg ${NS} viewBox="0 0 ${width} ${height}"/>`)}`, { display: 'block' });
    expect(Math.round(blank.getBoundingClientRect().height)).toBe(Math.round(box.height));
  });
});

// The fixtures internal/attachment/testdata holds, byte for byte, and the
// size TestAVIFSizeOfRealFiles asserts for each.
const AVIF_FIXTURES: [string, number, number, string][] = [
  ['solid-8x3.avif', 8, 3, 'AAAAHGZ0eXBhdmlmAAAAAG1pZjFhdmlmbWlhZgAAANZtZXRhAAAAAAAAACFoZGxyAAAAAAAAAABwaWN0AAAAAAAAAAAAAAAAAAAAACJpbG9jAAAAAERAAAEAAQAAAAAA+gABAAAAAAAAACQAAAAjaWluZgAAAAAAAQAAABVpbmZlAgAAAAABAABhdjAxAAAAAA5waXRtAAAAAAABAAAAVmlwcnAAAAA4aXBjbwAAAAxhdjFDgSACAAAAABRpc3BlAAAAAAAAAAgAAAADAAAAEHBpeGkAAAAAAwgICAAAABZpcG1hAAAAAAAAAAEAAQOBAgMAAAAsbWRhdBIACgg4CHzSAhoNIDIWGUJjBMPPPPNBIACQQRHVLCQRe7H9gA=='],
  ['solid-5x7.avif', 5, 7, 'AAAAHGZ0eXBhdmlmAAAAAG1pZjFhdmlmbWlhZgAAANZtZXRhAAAAAAAAACFoZGxyAAAAAAAAAABwaWN0AAAAAAAAAAAAAAAAAAAAACJpbG9jAAAAAERAAAEAAQAAAAAA+gABAAAAAAAAACYAAAAjaWluZgAAAAAAAQAAABVpbmZlAgAAAAABAABhdjAxAAAAAA5waXRtAAAAAAABAAAAVmlwcnAAAAA4aXBjbwAAAAxhdjFDgSACAAAAABRpc3BlAAAAAAAAAAUAAAAHAAAAEHBpeGkAAAAAAwgICAAAABZpcG1hAAAAAAAAAAEAAQOBAgMAAAAubWRhdBIACgg4CKZpAQ0GkDIYGUJjBMPPPPNBIACQQRHVRJXy8ZQnCnZa'],
  ['solid-64x1.avif', 64, 1, 'AAAAHGZ0eXBhdmlmAAAAAG1pZjFhdmlmbWlhZgAAANZtZXRhAAAAAAAAACFoZGxyAAAAAAAAAABwaWN0AAAAAAAAAAAAAAAAAAAAACJpbG9jAAAAAERAAAEAAQAAAAAA+gABAAAAAAAAACoAAAAjaWluZgAAAAAAAQAAABVpbmZlAgAAAAABAABhdjAxAAAAAA5waXRtAAAAAAABAAAAVmlwcnAAAAA4aXBjbwAAAAxhdjFDgSACAAAAABRpc3BlAAAAAAAAAEAAAAABAAAAEHBpeGkAAAAAAwgICAAAABZpcG1hAAAAAAAAAAEAAQOBAgMAAAAybWRhdBIACgg4FD80gIaDSDIcGUJjBMPPPPNBIED4/A93uBVWJbns7dYm7DklXg=='],
];

describe('an avif', () => {
  it.each(AVIF_FIXTURES)('%s decodes at %ix%i', async (_name, width, height, base64) => {
    const bytes = Uint8Array.from(atob(base64), (c) => c.charCodeAt(0));
    const url = URL.createObjectURL(new Blob([bytes], { type: 'image/avif' }));
    try {
      const img = await loaded(url);
      expect([img.naturalWidth, img.naturalHeight]).toEqual([width, height]);
    } finally {
      URL.revokeObjectURL(url);
    }
  });
});
