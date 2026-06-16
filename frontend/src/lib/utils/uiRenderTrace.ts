import { AppendUIRenderTraceBatch, BookmarkUIRenderTrace } from '../stores/bindings';
import { getActiveTurn } from '../stores/threadStatuses.svelte';
import type { ThreadPane } from '../stores/thread.svelte';
import { UI_TRACE_MAX_LINE_BYTES } from './uiTraceLimits';

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
// See uiTraceLimits.ts: keeps small per-event diagnostic traces from
// being collateral damage when a snapshot trace (chat.dom / chat.state)
// blows the per-line cap and Go rejects the whole batch.
const MAX_LINE_BYTES = UI_TRACE_MAX_LINE_BYTES;
const MAX_DOM_ROWS = 64;
const PREVIEW_CHARS = 80;
const DOM_TRACE_MIN_INTERVAL_MS = 250;
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

interface PendingDomTrace {
  label: string;
  build: () => unknown;
  frame: number | null;
  timer: ReturnType<typeof setTimeout> | null;
}

let enabled = initialEnabled();
let nextSeq = 1;
const records: UiTraceRecord[] = [];
const pendingFileLines: string[] = [];
const pendingDomTraces = new Map<string, PendingDomTrace>();
const lastDomTraceAtByKey = new Map<string, number>();
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
  for (const pending of pendingDomTraces.values()) {
    if (pending.frame !== null) cancelAnimationFrame(pending.frame);
    if (pending.timer !== null) clearTimeout(pending.timer);
  }
  pendingDomTraces.clear();
  lastDomTraceAtByKey.clear();
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
  const existing = pendingDomTraces.get(key);
  if (existing) {
    existing.label = label;
    existing.build = build;
    return;
  }

  const pending: PendingDomTrace = {
    label,
    build,
    frame: null,
    timer: null,
  };
  pendingDomTraces.set(key, pending);

  const scheduleFrame = () => {
    pending.timer = null;
    pending.frame = requestAnimationFrame(() => {
      pendingDomTraces.delete(key);
      lastDomTraceAtByKey.set(key, Date.now());
      recordUiTrace(pending.label, pending.build());
    });
  };

  const lastAt = lastDomTraceAtByKey.get(key) ?? Number.NEGATIVE_INFINITY;
  const delay = Math.max(0, DOM_TRACE_MIN_INTERVAL_MS - (Date.now() - lastAt));
  if (delay > 0) {
    pending.timer = setTimeout(scheduleFrame, delay);
  } else {
    scheduleFrame();
  }
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
  installBugReportHotkey();
}

// Ctrl+Shift+B drops a `user.bugReport` marker into the trace, force-
// flushes it to disk, and copies the trace file path to the clipboard.
// The user pastes the path; whoever's reading the report greps for
// `user.bugReport` to land on the bug moment with full surrounding
// context. Active only in DEBUG=1 builds.
function installBugReportHotkey(): void {
  if (!UI_TRACE_BUILD_GATE || typeof window === 'undefined') return;
  window.addEventListener('keydown', (e) => {
    // Match the lowercase key code so the binding works regardless of
    // shift's effect on `e.key`. `code` is the physical key.
    if (!(e.ctrlKey && e.shiftKey && e.code === 'KeyB')) return;
    e.preventDefault();
    void captureBugReport();
  });
}

async function captureBugReport(): Promise<void> {
  const win = window as Window & {
    __stickState?: () => Record<string, unknown>;
  };
  const stickState = win.__stickState?.() ?? null;
  // Marker trace event — searchable via `grep user.bugReport` on the
  // file. Includes the full stick state so the bug moment is
  // self-describing even without scanning surrounding events.
  recordUiTrace('user.bugReport', {
    capturedAt: Date.now(),
    href: typeof location !== 'undefined' ? location.href : '',
    stickState,
  });
  // Force-flush before bookmarking so the marker is on disk and the
  // bookmark file captures it. Without this, the in-memory pending
  // lines would copy-out only after the next scheduled flush and the
  // marker could miss the bookmark.
  await flushUiRenderTrace();
  // Take a frozen snapshot of the current trace file (plus any rotated
  // `.1` predecessor) so the bug-moment context survives the next
  // rotation triggered by ongoing render activity. The live trace can
  // turn over in minutes during a streaming session, but the bookmark
  // doesn't rotate — analysis can happen hours later.
  let bookmarkPath: string | null = null;
  try {
    bookmarkPath = await BookmarkUIRenderTrace();
  } catch (err) {
    console.warn('[BugReport] Failed to bookmark trace:', err);
  }
  const copyPath = bookmarkPath || lastTraceFilePath;
  if (copyPath && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(copyPath);
    } catch (err) {
      console.warn('[BugReport] Failed to copy path to clipboard:', err);
    }
  }
  const message = bookmarkPath
    ? `[BugReport] Bookmark saved. Path copied to clipboard:\n${bookmarkPath}`
    : lastTraceFilePath
      ? `[BugReport] Bookmark unavailable; live trace path copied to clipboard:\n${lastTraceFilePath}`
      : '[BugReport] Saved marker but no trace file path is available yet.';
  console.log(message);
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
    // The items array used to live here. It dominated the trace file
    // (single chat.state snapshot averaged ~45 KB on a 228-item thread,
    // burning ~25% of the 10 MB rotation cap on data that changes very
    // slowly between consecutive emissions). The DOM traces capture
    // rendered row identity and scroll geometry; row text is intentionally
    // omitted so DEBUG tracing does not walk every mounted row's text.
  };
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
