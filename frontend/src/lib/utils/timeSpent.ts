// Time spent as a person types it and as a time screen shows it. A
// tracker's worklog carries it in seconds.

/** What a time field says when it cannot read what was typed. */
export const TIME_SPENT_REFUSAL = 'Type the time as 1h 30m, 45m or a number of minutes.';

// Hours, which may be decimal, then whole minutes, either one optional;
// or a bare whole number of minutes.
const SPENT = /^(?:(\d+(?:\.\d+)?)\s*h)?\s*(?:(\d+)\s*m)?$/i;
const MINUTES = /^\d+$/;

/** The seconds that `text` spells ("1h 30m", "1h", "45m", "1.5h" or a
 * number of minutes), or null when it spells none, spells zero, or spells
 * a time that is not a whole number of minutes ("1.01h"). */
export function parseTimeSpent(text: string): number | null {
  const t = text.trim();
  let minutes: number;
  if (MINUTES.test(t)) {
    minutes = Number(t);
  } else {
    const m = SPENT.exec(t);
    if (!m || (m[1] === undefined && m[2] === undefined)) return null;
    minutes = Number(m[1] ?? 0) * 60 + Number(m[2] ?? 0);
  }
  // 4.15h is 249.00000000000003 minutes in floating point: a value within a
  // millionth of a minute is that minute.
  const whole = Math.round(minutes);
  if (whole <= 0 || Math.abs(minutes - whole) > 1e-6 || !Number.isSafeInteger(whole * 60)) return null;
  return whole * 60;
}

/** `seconds` as "1h 30m", "2h" or "45m", with any seconds that are not a
 * whole minute after them ("1m 30s"), so a value the tracker holds is
 * shown exactly; "0m" for none. */
export function formatTimeSpent(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const rest = s % 60;
  const parts: string[] = [];
  if (h > 0) parts.push(`${h}h`);
  if (m > 0) parts.push(`${m}m`);
  if (rest > 0) parts.push(`${rest}s`);
  return parts.length ? parts.join(' ') : '0m';
}
