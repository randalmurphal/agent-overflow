// Pure reconciliation policy. Wire patches can authoritatively replace text;
// snapshots can lag the displayed cursor. Callers choose the authority, while
// this comparison describes the text without mutating or duplicating the full received buffer.
import { isReasoningTailKind, THINKING_TAIL_RUNES, trimToTailRunes } from './threadPaneShared';
import { streamedTextPast, utf8Length } from '../utils/utf8Offsets';

export type RevealTextRelation = 'same' | 'extension' | 'trailing' | 'replacement';

export function classifyRevealText(
  kind: string | undefined,
  incoming: string,
  received: string,
): RevealTextRelation {
  if (incoming === received) return 'same';
  const reasoning = kind !== undefined && isReasoningTailKind(kind);
  if (reasoning && incoming === trimToTailRunes(received, THINKING_TAIL_RUNES)) return 'same';
  if (incoming.length > received.length && incoming.startsWith(received)) return 'extension';
  if (incoming.length < received.length &&
    (received.startsWith(incoming) || (reasoning && received.includes(incoming)))) return 'trailing';
  return 'replacement';
}

/**
 * `classifyRevealText` for two reads of one stream that carry positions:
 * `incoming` ends at `incomingEnd` and the received text at `receivedEnd`,
 * both in UTF-8 bytes of the streamed text (`Item.streamEnd`). Position
 * decides, not text, so a repeated phrase or a trimmed reasoning tail
 * cannot mislead it. `suffix` is the extension's text past `receivedEnd`.
 * A read that starts past the received text replaces it.
 */
export function classifyStreamedText(
  incoming: string,
  incomingEnd: number,
  receivedEnd: number,
): { relation: RevealTextRelation; suffix: string } {
  if (incomingEnd === receivedEnd) return { relation: 'same', suffix: '' };
  if (incomingEnd < receivedEnd) return { relation: 'trailing', suffix: '' };
  const suffix = streamedTextPast(incoming, incomingEnd - utf8Length(incoming), receivedEnd);
  return suffix === null ? { relation: 'replacement', suffix: '' } : { relation: 'extension', suffix };
}
