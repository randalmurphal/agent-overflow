// Tool panes: app-wide surfaces (Pull requests, Workflows) that live in the
// pane strip beside threads. A tool pane is a layout item and nothing else:
// it has no ThreadPane, no source pane and no registry entry, and there is at
// most one per kind, under `toolPaneIdFor(kind)`. Whether a tool is open is
// read off the layout, so this store owns only the tool used last.
//
// Opening follows the thread rule. A plain open replaces the focused pane in
// place: a focused thread pane closes through `destroyPane` (its companions
// cascade) and the tool takes its layout index; a focused companion resolves
// to the thread pane it belongs to, which is the one replaced; a focused tool
// pane gives its slot to the other tool. With nothing focused the tool is
// appended. A new-pane open inserts right of the focused pane and replaces
// nothing. The reverse direction, a thread opened while a tool pane is
// focused, is `panes.svelte#focusedOpenTargetPane`.
//
// panes.svelte.ts must not import this module. It notifies through
// `addToolPaneReplacedObserver`, armed by `installToolPanes`.

import {
  addPaneLayoutItem,
  averagePaneWidthPx,
  getPaneLayoutItems,
  isToolKind,
  notePaneLayoutMutation,
  paneLayoutKindOf,
  removePaneLayoutItem,
  sourcePaneIdOf,
  toolPaneIdFor,
  type ToolPaneKind,
} from './paneLayout.svelte';
import {
  addToolPaneReplacedObserver,
  destroyPane,
  focusPane,
  getFocusedPaneId,
  getPane,
  removeToolPaneFromLayout,
  resolveNewPaneInsertIndex,
  revealPane,
} from './panes.svelte';
import { appStorageGet, appStorageSet } from './appStorage';

export type { ToolPaneKind } from './paneLayout.svelte';

const LAST_TOOL_KEY = 'tools:last';
const DEFAULT_TOOL: ToolPaneKind = 'pulls';

function parseLastTool(raw: string | null): ToolPaneKind {
  return isToolKind(raw) ? raw : DEFAULT_TOOL;
}

let lastTool = $state<ToolPaneKind>(parseLastTool(appStorageGet(LAST_TOOL_KEY)));

/** The tool kind of a mounted tool pane, null for any other id. */
export function toolKindOf(paneId: string | null): ToolPaneKind | null {
  if (!paneId) return null;
  const kind = paneLayoutKindOf(paneId);
  return isToolKind(kind) ? kind : null;
}

/** The tool kind of the RAW focused pane, null when focus is not a tool. */
export function focusedToolKind(): ToolPaneKind | null {
  return toolKindOf(getFocusedPaneId());
}

export function isToolOpen(kind: ToolPaneKind): boolean {
  return paneLayoutKindOf(toolPaneIdFor(kind)) === kind;
}

/** The tool opened or focused last, persisted per client. */
export function lastToolKind(): ToolPaneKind {
  return lastTool;
}

/** The sidebar's labelled tool: the focused tool pane, else the last used. */
export function activeToolKind(): ToolPaneKind {
  return focusedToolKind() ?? lastTool;
}

/** Record use of a tool. ToolPane calls it whenever its pane takes focus. */
export function noteToolUsed(kind: ToolPaneKind): void {
  if (lastTool === kind) return;
  lastTool = kind;
  appStorageSet(LAST_TOOL_KEY, kind);
}

/** Adopt the durable copy once appStorage hydration lands (App.svelte boot). */
export function syncToolPanesFromAppStorage(): void {
  lastTool = parseLastTool(appStorageGet(LAST_TOOL_KEY));
}

/**
 * Settings is a layer over the pane strip, so a tool opened under it would be
 * invisible. Injected by the settings store at its module init, which keeps
 * the import one-way (settings → this module).
 */
let closeSettingsSurface: () => void = () => {};

export function setToolPaneOpenExclusion(closeSettings: () => void): void {
  closeSettingsSurface = closeSettings;
}

type ToolPaneClosedObserver = (kind: ToolPaneKind) => void;
let closedObservers: readonly ToolPaneClosedObserver[] = [];

/**
 * Observe a tool pane leaving the layout: closed, or replaced in place by a
 * thread open. A tool store clears view state that must not outlive its pane
 * (an open dialog, an armed confirm).
 */
export function addToolPaneClosedObserver(observer: ToolPaneClosedObserver): () => void {
  closedObservers = [...closedObservers, observer];
  return () => {
    closedObservers = closedObservers.filter((existing) => existing !== observer);
  };
}

function notifyToolPaneClosed(kind: ToolPaneKind): void {
  for (const observe of closedObservers) observe(kind);
}

// Each tool reports whether a detail step is open above its list, so the
// `toolDetailOpen` command flag can gate the tool's surface-dismissal command:
// Escape and the phone's Back step back to the list before they leave the
// pane. A tool with no probe has no detail step.
const detailProbes = new Map<ToolPaneKind, () => boolean>();

export function registerToolDetailProbe(kind: ToolPaneKind, isOpen: () => boolean): void {
  detailProbes.set(kind, isOpen);
}

/** The focused tool pane has a detail step open above its list. */
export function focusedToolDetailOpen(): boolean {
  const kind = focusedToolKind();
  return kind !== null && (detailProbes.get(kind)?.() ?? false);
}

let unsubscribeReplaced: (() => void) | null = null;

export function installToolPanes(): void {
  unsubscribeReplaced?.();
  unsubscribeReplaced = addToolPaneReplacedObserver((_paneId, kind) => notifyToolPaneClosed(kind));
}

/**
 * The layout block the focused pane belongs to: its lead's id, index and
 * width. A companion walks its source chain to the thread pane it hangs from.
 * Null when nothing in the layout holds focus.
 */
function focusedBlockLead(): { paneId: string; index: number; widthPx: number } | null {
  let paneId = getFocusedPaneId();
  if (!paneId) return null;
  const items = getPaneLayoutItems();
  for (let hops = 0; hops < items.length; hops += 1) {
    const source = sourcePaneIdOf(paneId);
    if (!source) break;
    paneId = source;
  }
  const index = items.findIndex((item) => item.paneId === paneId);
  if (index < 0) return null;
  return { paneId, index, widthPx: items[index].widthPx };
}

export interface OpenToolOptions {
  /** Insert a new pane right of the focused pane instead of replacing it. */
  newPane?: boolean;
}

export function openTool(kind: ToolPaneKind, options: OpenToolOptions = {}): void {
  closeSettingsSurface();
  noteToolUsed(kind);
  const paneId = toolPaneIdFor(kind);
  if (isToolOpen(kind)) {
    focusPane(paneId);
    revealPane(paneId);
    return;
  }
  notePaneLayoutMutation();
  let insertIndex: number | undefined;
  let widthPx = averagePaneWidthPx();
  if (options.newPane) {
    insertIndex = resolveNewPaneInsertIndex();
  } else {
    const lead = focusedBlockLead();
    if (lead) {
      insertIndex = lead.index;
      widthPx = lead.widthPx;
      replaceLead(lead.paneId);
    }
  }
  addPaneLayoutItem({ id: paneId, paneId, kind, widthPx }, insertIndex);
  focusPane(paneId);
  revealPane(paneId);
}

// The lead and everything after it in its block leave together, so the lead's
// index is still the slot the tool takes once they are gone.
function replaceLead(leadPaneId: string): void {
  const toolKind = toolKindOf(leadPaneId);
  if (toolKind) {
    removePaneLayoutItem(leadPaneId, { persist: false });
    notifyToolPaneClosed(toolKind);
  } else if (getPane(leadPaneId)) {
    destroyPane(leadPaneId);
  } else {
    // A layout item with no registry pane renders "Pane unavailable"; it has
    // nothing to tear down.
    removePaneLayoutItem(leadPaneId, { persist: false });
  }
}

/** Close a tool pane. Focus moves to the adjacent pane. */
export function closeToolPane(paneId: string): void {
  const kind = toolKindOf(paneId);
  if (!kind) return;
  if (removeToolPaneFromLayout(paneId)) notifyToolPaneClosed(kind);
}

/** `workflows.toggle` and its kin: open or reveal, or close when focused. */
export function toggleTool(kind: ToolPaneKind): void {
  if (focusedToolKind() === kind) closeToolPane(toolPaneIdFor(kind));
  else openTool(kind);
}

export function resetToolPanesForTest(): void {
  lastTool = DEFAULT_TOOL;
  unsubscribeReplaced?.();
  unsubscribeReplaced = null;
}
