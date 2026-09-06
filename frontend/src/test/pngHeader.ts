// Header-only PNG for tests that replace the browser decoder. Never a fixture
// for a real browser: it deliberately has no pixel data or complete checksum.
export function pngHeader(width: number, height: number): string {
  const bytes = new Uint8Array(33);
  bytes.set([137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82]);
  const view = new DataView(bytes.buffer);
  view.setUint32(16, width);
  view.setUint32(20, height);
  return btoa(String.fromCharCode(...bytes));
}
