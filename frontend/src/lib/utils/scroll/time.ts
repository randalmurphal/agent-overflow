// Shared elapsed-time source for scroll events and animation steps.
// Date.now() keeps contexts without performance functional.
export function nowMs(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}
