import { deflateSync } from 'node:zlib';

// A 300x200 truecolour PNG of two flat bands: past the thumbnail cap in
// both axes, and still 494 bytes because flat bands deflate to nothing.
export const PNG_BASE64 =
  'iVBORw0KGgoAAAANSUhEUgAAASwAAADICAIAAADdvUsCAAABtUlEQVR42u3TAQkAAAjAMLOYxXCGM5QxRBgsweGRPc' +
  'ChkABMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYE' +
  'TAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBBOqAC' +
  'YEEwImBBMCJgQTAiYEEwImBBMCJgQTAiYEEwImBBMCJgQTAiYEEwImBBMCJgQTAiYEEwImBBMCJgQTAiYEEwImhNcT' +
  'TiVwyIRgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQj' +
  'AhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIZhQ' +
  'AjAhmBAwIZgQMCGYEDAhmBAwIZgQMCGYEDAhmBAwIZgQMCGYEDAhmBAwIZgQMCGYEDAhmBAwIZgQMCGYEDAhmBAwIb' +
  'y2ay5ZumneNVwAAAAASUVORK5CYII=';
export const PNG_BYTES = Buffer.from(PNG_BASE64, 'base64');
export const PNG_WIDTH = 300;
export const PNG_HEIGHT = 200;

// Minimal truecolour PNG encoder: one flat colour, so the file is a few
// hundred bytes regardless of its dimensions.
const CRC_TABLE = new Uint32Array(256).map((_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});

function crc32(buf: Buffer): number {
  let c = 0xffffffff;
  for (const byte of buf) c = CRC_TABLE[(c ^ byte) & 0xff]! ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function pngChunk(type: string, data: Buffer): Buffer {
  const length = Buffer.alloc(4);
  length.writeUInt32BE(data.length, 0);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body), 0);
  return Buffer.concat([length, body, crc]);
}

export function solidPng(width: number, height: number, rgb: [number, number, number]): Buffer {
  const row = Buffer.alloc(1 + width * 3);
  for (let x = 0; x < width; x++) {
    row[1 + x * 3] = rgb[0];
    row[2 + x * 3] = rgb[1];
    row[3 + x * 3] = rgb[2];
  }
  const raw = Buffer.concat(Array.from({ length: height }, () => row));
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // truecolour
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk('IHDR', ihdr),
    pngChunk('IDAT', deflateSync(raw)),
    pngChunk('IEND', Buffer.alloc(0)),
  ]);
}
