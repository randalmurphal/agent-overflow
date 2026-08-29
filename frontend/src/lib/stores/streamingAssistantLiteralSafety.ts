const LITERAL_TEXT = /^[\p{L}\p{M}\p{N} ,.?!:']+$/u;

function isWordCodeUnit(code: number): boolean {
  return (code >= 48 && code <= 57) ||
    (code >= 65 && code <= 90) ||
    (code >= 97 && code <= 122) ||
    code >= 128;
}

/**
 * Code units that can prove the rendered literal tail of an authoritative
 * delta. Markdown's structural delimiters are ASCII, so non-ASCII code units
 * are safe tail evidence even when the stricter direct-delta grammar declines
 * a punctuation category it does not recognize.
 */
export function isAssistantLiteralTailCodeUnit(code: number): boolean {
  return isWordCodeUnit(code) || code === 32 || code === 33 || code === 39 ||
    code === 44 || code === 46 || code === 58 || code === 63;
}

/**
 * Admit punctuation only when this reveal unit proves it is ordinary prose.
 * A punctuation byte at a provider-chunk edge can still become markdown when
 * the next chunk arrives. Requiring its following space in the same unit makes
 * that decision final. Period after a digit stays authoritative because it can
 * complete an ordered-list marker. Apostrophes are safe only inside a word.
 */
export function isSafeAssistantLiteralDelta(delta: string): boolean {
  if (!LITERAL_TEXT.test(delta)) return false;
  for (let index = 0; index < delta.length; index++) {
    const code = delta.charCodeAt(index);
    if (code === 39) {
      if (
        index === 0 ||
        index + 1 >= delta.length ||
        !isWordCodeUnit(delta.charCodeAt(index - 1)) ||
        !isWordCodeUnit(delta.charCodeAt(index + 1))
      ) return false;
      continue;
    }
    if (code !== 44 && code !== 46 && code !== 58 && code !== 63 && code !== 33) {
      continue;
    }
    if (
      index === 0 ||
      index + 1 >= delta.length ||
      delta.charCodeAt(index + 1) !== 32 ||
      !isWordCodeUnit(delta.charCodeAt(index - 1)) ||
      (code === 46 && delta.charCodeAt(index - 1) >= 48 && delta.charCodeAt(index - 1) <= 57)
    ) return false;
  }
  return true;
}

export function isSafeAssistantLiteralPredecessor(code: number): boolean {
  return code === -1 || code === 32 ||
    (code >= 48 && code <= 57) ||
    (code >= 65 && code <= 90) ||
    (code >= 97 && code <= 122) ||
    code >= 128;
}
