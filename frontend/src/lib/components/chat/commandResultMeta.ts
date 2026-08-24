// Decoder for the `command_result` row's `items.meta` blob.
//
// Wire shape (internal/triage/command_result.go, `commandResultMeta`):
//
//   {kind: "command_result", preview: string,
//    truncated?: boolean, totalBytes?: number}
//
// `preview` is the bounded head of the output and IS the whole output when
// `truncated` is absent/false — that case never needs a payload fetch. When
// truncated, the full bytes live in a `command_result` payload the row loads
// on demand.
//
// Pure and separate from the component so the "is there anything left to
// fetch" decision is testable without a DOM, and so the row template can stay
// a thin read of one derived record.

import type { Item } from '../../types/models';
import { parseJsonObject } from '../../utils/parseJsonObject';
import { commandAgentResult, type CommandAgentResult } from '../../utils/commandAgentResult';

export interface CommandResultView {
  /**
   * Text to render immediately. The whole output when `truncated` is false;
   * the bounded head otherwise.
   */
  preview: string;
  /**
   * True only when there is genuinely more output AND it is reachable — the
   * meta's truncated flag AND a linked payload. Keeping the payload check
   * inside the predicate is what stops the row offering a "show full output"
   * affordance that resolves to a failed fetch: every consumer asking
   * "is more available?" gets one answer, and a meta/payload disagreement
   * degrades to the honest inline-only row.
   */
  truncated: boolean;
  /**
   * Full output length in bytes, for the load affordance's size label. 0 when
   * the wire carried none (it is written only alongside `truncated`).
   */
  totalBytes: number;
  /** Forked-agent source. Null for ordinary terminal-style command output. */
  agentResult: CommandAgentResult | null;
}

/**
 * Project a `command_result` item onto what the row renders.
 *
 * Defensive by construction: a row whose meta is missing, unparseable, or
 * shaped differently by an older/newer backend still renders its summary,
 * which triage writes as the same bounded preview.
 */
export function readCommandResultView(item: Item): CommandResultView {
  const meta = parseJsonObject(item.meta);
  const metaPreview = typeof meta?.preview === 'string' ? meta.preview : '';
  const rawTotal = meta?.totalBytes;
  const truncated = meta?.truncated === true && Boolean(item.payloadId);
  return {
    preview: metaPreview || item.summary || '',
    truncated,
    totalBytes:
      truncated && typeof rawTotal === 'number' && Number.isFinite(rawTotal) && rawTotal > 0
        ? rawTotal
        : 0,
    agentResult: commandAgentResult(item),
  };
}
