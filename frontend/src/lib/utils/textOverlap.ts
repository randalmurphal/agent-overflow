export function nonOverlappingSuffix(existing: string, delta: string): string {
  if (!existing || !delta) return delta;
  const maxOverlap = Math.min(existing.length, delta.length);
  for (let overlap = maxOverlap; overlap > 0; overlap -= 1) {
    if (existing.endsWith(delta.slice(0, overlap))) {
      return delta.slice(overlap);
    }
  }
  return delta;
}
