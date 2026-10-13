/**
 * A wake (a loop's fire, a watch's output, a worker's prompt, an outside
 * `thread send`) quotes each piece of data as a Go string literal
 * (internal/untrustedtext): ASCII only, with `<`, `>` and `&` as `\u`
 * escapes, which is what keeps the agent reading it as data. A person reads
 * the same message, so each quoted token is shown with its escapes decoded.
 * An escaped quote keeps its escape, so a token still reads as one value,
 * and so does any control, format (bidi, zero-width) or line-separator
 * character, so nothing in the data reorders, hides or breaks the text
 * around it.
 */

const QUOTED_TOKEN = /"(?:[^"\\\n]|\\.)*"/g;
const ESCAPE = /\\(?:x([0-9a-fA-F]{2})|u([0-9a-fA-F]{4})|U([0-9a-fA-F]{8})|(.))/g;
const KEEP_ESCAPED = /[\p{Cc}\p{Cf}\p{Cs}\p{Zl}\p{Zp}]/u;

/**
 * The send-id roots of messages the app wrote for a thread rather than a
 * person: agentWakeSendIDRoot, workerPromptSendIDRoot and agentSendIDRoot
 * in internal/app.
 */
const WAKE_SEND_ID_ROOTS = ['agent-wake:', 'agent-ask:', 'agent-send:'] as const;

/** Whether a queued message with this send id is a wake. */
export function isWakeSendId(sendId: string | undefined): boolean {
  return sendId !== undefined && WAKE_SEND_ID_ROOTS.some((root) => sendId.startsWith(root));
}

/** A wake's text with its quoted data decoded for reading. */
export function readableWakeText(text: string): string {
  return text.replace(QUOTED_TOKEN, (token) => token.replace(ESCAPE, decodeEscape));
}

function decodeEscape(
  escape: string,
  hex2: string | undefined,
  hex4: string | undefined,
  hex8: string | undefined,
  single: string | undefined,
): string {
  if (single !== undefined) return single === '\\' ? '\\' : escape;
  const code = Number.parseInt(hex2 ?? hex4 ?? hex8 ?? '', 16);
  if (!(code <= 0x10ffff)) return escape;
  const char = String.fromCodePoint(code);
  return KEEP_ESCAPED.test(char) ? escape : char;
}
