import { isUiRenderTraceEnabled, recordUiTrace } from '../uiRenderTrace';

// Diagnostic trace helper for the scroll modules — no-op in production
// (gated by `isUiRenderTraceEnabled` which only returns true in dev with
// `VITE_AGENT_OVERFLOW_UI_TRACE=1`, set by `make dev DEBUG=1`). The
// thunk skips object construction when disabled. Records flow into
// `${configDir}/ui-trace/ui-render.jsonl` via the same batched
// `AppendUIRenderTraceBatch` binding the timeline render trace uses.
//
// Call sites double-gate with `if (isUiRenderTraceEnabled()) trace(…)`
// because Rolldown inlines the gate but does not eliminate the closure
// allocation — the outer guard prevents ~120 closures/sec during
// spring animation in production builds.
export function trace(label: string, build: () => Record<string, unknown>): void {
  if (!isUiRenderTraceEnabled()) return;
  recordUiTrace(label, build());
}
