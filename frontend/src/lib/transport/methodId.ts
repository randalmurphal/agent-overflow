/**
 * A bound App method's wire id: FNV-1a 32 of `main.App.<name>`, as the
 * dispatcher registers it (internal/transport/dispatcher.go, fnvHash) and
 * the generated bindings call it.
 */
export function methodId(name: string): number {
  const text = `main.App.${name}`;
  let hash = 0x811c9dc5;
  for (let i = 0; i < text.length; i++) {
    hash ^= text.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  return hash >>> 0;
}
