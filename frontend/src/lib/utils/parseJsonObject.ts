// Defensive JSON.parse for backend-supplied "JSON-string-or-undefined"
// fields like `Item.payloadMeta` and `Item.meta`. Garbage strings,
// non-object roots (numbers, arrays, "null"), and parse errors all
// fall through to `null` so callers don't have to reason about them.

export function parseJsonObject(raw: string | undefined | null): Record<string, unknown> | null {
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return parsed as Record<string, unknown>;
    }
    return null;
  } catch {
    return null;
  }
}
