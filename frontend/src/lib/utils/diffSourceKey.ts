import { FNV_OFFSET_BASIS_32 } from './fnv1a';

export function diffSourceKey(text: string): string {
  let hash = FNV_OFFSET_BASIS_32;
  for (let index = 0; index < text.length; index += 1) {
    hash ^= text.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return `fnv1a:${hash.toString(16).padStart(8, '0')}:${text.length}`;
}
