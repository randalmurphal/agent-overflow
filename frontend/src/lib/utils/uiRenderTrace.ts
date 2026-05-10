import { AppendUIRenderTraceBatch } from '../stores/bindings';
import { getActiveTurn } from '../stores/threadStatuses.svelte';
import type { ThreadPane } from '../stores/thread.svelte';
import type { Item } from '../types/models';

// The trace surface is opt-in at build time via VITE_AGENT_OVERFLOW_UI_TRACE
// (set by `make dev DEBUG=1` / `make dev-wsl DEBUG=1`). Vite inlines the
// env var at build time as a string literal — production builds without
// the env var set leave UI_TRACE_BUILD_GATE === false, so the entire
// surface dead-code-eliminates. We previously gated on `import.meta.env.DEV`,
// but the WSL launcher path runs the `build:dev` output as a Wails-built
// binary and `import.meta.env.DEV` was returning false there. The env var
// is the explicit user-controlled signal regardless of build mode.
//
// Test mode also enables the gate so unit tests can flip `enabled` on
// and off via the public setter.
const UI_TRACE_BUILD_GATE: boolean =
  import.meta.env.VITE_AGENT_OVERFLOW_UI_TRACE === '1' ||
  import.meta.env.MODE === 'test';

const MAX_RECORDS = 500;
const MAX_PENDING_FILE_LINES = 200;
// Mirror app_ui_trace.go's `uiTraceMaxLineBytes`. The Go side rejects
// the WHOLE batch on a single oversized line, so we filter client-side
// to keep small per-event diagnostic traces from being collateral
// damage when a snapshot trace (chat.dom / chat.state) blows the cap.
const MAX_LINE_BYTES = 64 * 1024;
const MAX_ITEMS = 120;
const MAX_DOM_ROWS = 160;
const PREVIEW_CHARS = 120;
const FILE_FLUSH_DELAY_MS = 500;

export interface UiTraceRecord {
  seq: number;
  at: number;
  label: string;
  data: unknown;
}

interface UiTraceApi {
  enable(): void;
  disable(): void;
  enabled(): boolean;
  clear(): void;
  records(): UiTraceRecord[];
  recent(count?: number): UiTraceRecord[];
  dump(count?: number): string;
  flush(): Promise<string | null>;
  filePath(): string | null;
}

declare global {
  interface Window {
    __agentOverflowUiTrace?: UiTraceApi;
  }
}

let enabled = initialEnabled();
let nextSeq = 1;
const records: UiTraceRecord[] = [];
const pendingFileLines: string[] = [];
const pendingDomFrames = new Map<string, number>();
let fileFlushTimer: number | null = null;
let lastTraceFilePath: string | null = null;

function initialEnabled(): boolean {
  // When the build gate is true, the env var was set at build time —
  // tracing is on by default. The localStorage toggle only matters in
  // builds where the env var is intentionally not set; with the env-var
  // gate, that path is unreachable.
  return UI_TRACE_BUILD_GATE;
}

export function isUiRenderTraceEnabled(): boolean {
  return UI_TRACE_BUILD_GATE && enabled;
}

export function setUiRenderTraceEnabled(next: boolean): void {
  if (!UI_TRACE_BUILD_GATE) return;
  enabled = next;
  if (!next) {
    void flushUiRenderTrace();
  }
}

export function clearUiRenderTrace(): void {
  records.length = 0;
  pendingFileLines.length = 0;
  if (fileFlushTimer !== null) {
    clearTimeout(fileFlushTimer);
    fileFlushTimer = null;
  }
}

export function getUiRenderTraceRecords(): UiTraceRecord[] {
  return records.slice();
}

export function recordUiTrace(label: string, data: unknown): void {
  if (!isUiRenderTraceEnabled()) return;
  const record: UiTraceRecord = {
    seq: nextSeq++,
    at: Date.now(),
    label,
    data,
  };
  records.push(record);
  if (records.length > MAX_RECORDS) {
    records.splice(0, records.length - MAX_RECORDS);
  }
  queueTraceRecordForFile(record);
}

export function scheduleDomUiTrace(
  key: string,
  label: string,
  build: () => unknown,
): void {
  if (!isUiRenderTraceEnabled()) return;
  if (pendingDomFrames.has(key)) return;
  const frame = requestAnimationFrame(() => {
    pendingDomFrames.delete(key);
    recordUiTrace(label, build());
  });
  pendingDomFrames.set(key, frame);
}

export function installUiRenderTraceApi(): void {
  if (!UI_TRACE_BUILD_GATE || typeof window === 'undefined') return;
  window.__agentOverflowUiTrace = {
    enable: () => setUiRenderTraceEnabled(true),
    disable: () => setUiRenderTraceEnabled(false),
    enabled: () => isUiRenderTraceEnabled(),
    clear: clearUiRenderTrace,
    records: getUiRenderTraceRecords,
    recent: (count = 50) => records.slice(Math.max(0, records.length - count)),
    dump: (count = MAX_RECORDS) => JSON.stringify(
      records.slice(Math.max(0, records.length - count)),
      null,
      2,
    ),
    flush: flushUiRenderTrace,
    filePath: () => lastTraceFilePath,
  };
}

export async function flushUiRenderTrace(): Promise<string | null> {
  if (!UI_TRACE_BUILD_GATE || pendingFileLines.length === 0) {
    return lastTraceFilePath;
  }
  if (fileFlushTimer !== null) {
    clearTimeout(fileFlushTimer);
    fileFlushTimer = null;
  }

  const lines = pendingFileLines.splice(0, pendingFileLines.length);
  try {
    lastTraceFilePath = await AppendUIRenderTraceBatch(lines);
  } catch (error) {
    console.warn('Failed to write UI render trace batch', error);
  }
  return lastTraceFilePath;
}

function queueTraceRecordForFile(record: UiTraceRecord): void {
  const line = stringifyTraceRecord(record);
  if (!line) return;
  // Oversize lines are replaced with a stub placeholder so the
  // existence of the dropped record is still visible in the trace
  // file, but the batch sent to Go stays under the per-line cap.
  // Without this, one fat snapshot trace blows AppendUIRenderTraceBatch
  // for the entire batch (validateUITraceLines fails fast on the first
  // offender) and we lose dozens of small adjacent diagnostic traces.
  const safeLine = line.length <= MAX_LINE_BYTES
    ? line
    : stubLineForOversizeRecord(record, line.length);
  if (!safeLine) return;
  pendingFileLines.push(safeLine);
  if (pendingFileLines.length > MAX_PENDING_FILE_LINES) {
    pendingFileLines.splice(0, pendingFileLines.length - MAX_PENDING_FILE_LINES);
  }
  scheduleFileFlush();
}

function stringifyTraceRecord(record: UiTraceRecord): string | null {
  try {
    return JSON.stringify(record);
  } catch (error) {
    console.warn('Failed to serialize UI render trace record', error);
    return null;
  }
}

function stubLineForOversizeRecord(
  record: UiTraceRecord,
  originalBytes: number,
): string | null {
  return stringifyTraceRecord({
    seq: record.seq,
    at: record.at,
    label: record.label,
    data: { __droppedOversize: true, originalBytes, maxBytes: MAX_LINE_BYTES },
  });
}

function scheduleFileFlush(): void {
  if (!UI_TRACE_BUILD_GATE || fileFlushTimer !== null || typeof window === 'undefined') return;
  fileFlushTimer = window.setTimeout(() => {
    fileFlushTimer = null;
    void flushUiRenderTrace();
  }, FILE_FLUSH_DELAY_MS);
}

export function summarizePaneForTrace(pane: ThreadPane): Record<string, unknown> {
  return {
    threadId: pane.threadId,
    provider: pane.thread?.provider ?? null,
    mode: pane.thread?.mode ?? null,
    loading: pane.loading,
    itemCount: pane.items.length,
    timelineRevision: pane.timelineRevision,
    oldestLoadedTurnIndex: pane.oldestLoadedTurnIndex,
    hasMoreHistory: pane.hasMoreHistory,
    loadingOlder: pane.loadingOlder,
    activeTurn: ((): { turnId: string; turnIndex: number; startedAt: number } | null => {
      const turn = getActiveTurn(pane.threadId);
      return turn
        ? { turnId: turn.turnId, turnIndex: turn.turnIndex, startedAt: turn.startedAt }
        : null;
    })(),
    latestSettledTurn: pane.latestSettledTurn
      ? {
          turnId: pane.latestSettledTurn.turnId,
          turnIndex: pane.latestSettledTurn.turnIndex,
          assistantMessageId: pane.latestSettledTurn.assistantMessageId,
          stopReason: pane.latestSettledTurn.stopReason,
        }
      : null,
    pendingApprovals: pane.pendingApprovals.map((approval) => ({
      requestId: approval.requestId,
      threadId: approval.threadId,
      kind: approval.kind ?? '',
      toolName: approval.toolName,
    })),
    showTerminal: pane.showTerminal,
    showPlanSidebar: pane.showPlanSidebar,
    diffPanelOpen: pane.diffPanel.open,
    diffSidebarPayloadId: pane.activeDiffPayload?.payloadId ?? null,
    diffSidebarFilePath: pane.activeDiffPayload?.filePath ?? null,
    items: summarizeItemsForTrace(pane.items),
  };
}

export function summarizeItemsForTrace(items: Item[]): Array<Record<string, unknown>> {
  return items.slice(0, MAX_ITEMS).map((item) => ({
    id: item.id,
    threadId: item.threadId,
    turnIndex: item.turnIndex,
    itemIndex: item.itemIndex,
    kind: item.kind,
    role: item.role,
    status: item.status,
    parentId: item.parentId ?? '',
    completionOf: item.completionOf ?? '',
    payloadKind: item.payloadKind ?? '',
    isBackground: item.isBackground ?? false,
    summaryPreview: preview(item.summary),
  }));
}

export function snapshotChatDomForTrace(root: HTMLElement | undefined): Record<string, unknown> {
  if (!root) {
    return { mounted: false };
  }
  return {
    mounted: true,
    threadId: root.dataset.threadId ?? '',
    loadingVisible: root.querySelector('[role="status"]')?.textContent?.trim() ?? '',
    timelineRows: Array.from(root.querySelectorAll<HTMLElement>('[data-item-id]'))
      .slice(0, MAX_DOM_ROWS)
      .map((el) => ({
        itemId: el.dataset.itemId ?? '',
        textPreview: preview(el.textContent ?? ''),
      })),
    approvalCount: root.querySelectorAll('[data-testid="approval-card"]').length,
    activityRailMounted: root.querySelector('[data-testid="activity-rail"]') !== null,
    backgroundRows: Array.from(root.querySelectorAll<HTMLElement>('[data-testid="background-task-tray-row"]'))
      .map((el) => ({
        rowId: el.dataset.rowId ?? '',
        status: el.querySelector<HTMLElement>('[data-testid="background-task-tray-row-status"]')?.dataset.status ?? '',
      })),
    workingIndicator: snapshotTestIdText(root, 'activity-rail-working'),
    composerDisabled: root.querySelector<HTMLTextAreaElement>('textarea')?.disabled ?? null,
    scroll: snapshotScroll(root.querySelector<HTMLElement>('[data-testid="message-timeline-scroll"]')),
  };
}

function snapshotTestIdText(root: HTMLElement, testId: string): string {
  const el = root.querySelector<HTMLElement>(`[data-testid="${testId}"]`);
  return el ? preview(el.textContent ?? '') : '';
}

function snapshotScroll(el: HTMLElement | null): Record<string, number> | null {
  if (!el) return null;
  return {
    scrollTop: Math.round(el.scrollTop),
    scrollHeight: Math.round(el.scrollHeight),
    clientHeight: Math.round(el.clientHeight),
  };
}

function preview(value: string): string {
  const normalized = value.replace(/\s+/g, ' ').trim();
  if (normalized.length <= PREVIEW_CHARS) return normalized;
  return `${normalized.slice(0, PREVIEW_CHARS - 1).trimEnd()}...`;
}
