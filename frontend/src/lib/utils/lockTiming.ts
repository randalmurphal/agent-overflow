export const DEFAULT_LOCK_WINDOW_MS = 5 * 60_000;

/** A cold start or a clock moving backwards always requires authentication. */
export function shouldLock(lastPausedAt: number | null, now: number, windowMs: number): boolean {
  return lastPausedAt === null || windowMs <= 0 || lastPausedAt > now || now - lastPausedAt >= windowMs;
}
