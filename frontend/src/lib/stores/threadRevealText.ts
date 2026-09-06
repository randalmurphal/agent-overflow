// Pure reconciliation policy. Wire patches can authoritatively replace text;
// snapshots can lag the displayed cursor. Callers choose the authority, while
// this comparison describes the text without mutating or duplicating the full received buffer.
import { isReasoningTailKind, THINKING_TAIL_RUNES, trimToTailRunes } from './threadPaneShared';

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
