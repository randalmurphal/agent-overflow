// Shared copy/save controller for proposed plans. Both the inline
// ProposedPlanCard and the PlanSidebar mount their own copy/save buttons
// (each with its own modal lifetime), so each gets its own controller
// instance via this factory — runes inside the factory back the per-instance
// reactive state.

import { addToast } from '../stores/toast.svelte';
import { copyToClipboard } from './clipboard';
import { WriteThreadWorkspaceFile } from '../stores/bindings';
import { buildProposedPlanMarkdownFilename, normalizePlanMarkdownForExport } from './proposedPlan';

export interface ProposedPlanExport {
  readonly copied: boolean;
  readonly savePath: string;
  readonly saving: boolean;
  readonly saveDialogOpen: boolean;
  handleCopy(): Promise<void>;
  openSaveDialog(): Promise<void>;
  handleSave(): Promise<void>;
  closeSaveDialog(): void;
  setSavePath(value: string): void;
  dispose(): void;
}

export function createProposedPlanExport(
  getMarkdown: () => Promise<string>,
  getThreadId: () => string | null,
): ProposedPlanExport {
  let copied = $state(false);
  let savePath = $state('');
  let saving = $state(false);
  let saveDialogOpen = $state(false);
  let copyTimer: ReturnType<typeof setTimeout> | undefined;

  async function handleCopy(): Promise<void> {
    const markdown = await getMarkdown();
    if (!markdown) return;
    const ok = await copyToClipboard(normalizePlanMarkdownForExport(markdown));
    if (!ok) {
      addToast('error', 'Failed to copy plan');
      return;
    }
    copied = true;
    clearTimeout(copyTimer);
    copyTimer = setTimeout(() => { copied = false; }, 2000);
  }

  async function openSaveDialog(): Promise<void> {
    const markdown = await getMarkdown();
    if (!markdown) return;
    savePath = savePath.trim() || buildProposedPlanMarkdownFilename(markdown);
    saveDialogOpen = true;
  }

  async function handleSave(): Promise<void> {
    const tid = getThreadId();
    if (!tid || saving) return;
    const relativePath = savePath.trim();
    if (!relativePath) {
      addToast('warning', 'Enter a workspace-relative path');
      return;
    }
    const markdown = await getMarkdown();
    if (!markdown) return;

    saving = true;
    try {
      const writtenPath = await WriteThreadWorkspaceFile(
        tid,
        relativePath,
        normalizePlanMarkdownForExport(markdown),
      );
      addToast('success', `Plan saved to ${writtenPath}`);
      saveDialogOpen = false;
    } catch (err) {
      console.error('Failed to save proposed plan:', err);
      addToast('error', err instanceof Error ? err.message : 'Failed to save plan');
    } finally {
      saving = false;
    }
  }

  function closeSaveDialog(): void {
    // Don't close mid-save — the RPC is still in flight and the modal needs
    // to stay open until the write resolves.
    if (saving) return;
    saveDialogOpen = false;
  }

  function setSavePath(value: string): void {
    savePath = value;
  }

  function dispose(): void {
    clearTimeout(copyTimer);
  }

  return {
    get copied() { return copied; },
    get savePath() { return savePath; },
    get saving() { return saving; },
    get saveDialogOpen() { return saveDialogOpen; },
    handleCopy,
    openSaveDialog,
    handleSave,
    closeSaveDialog,
    setSavePath,
    dispose,
  };
}
